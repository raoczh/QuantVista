package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// 阶段 D 自选/持仓异动守护推送：不依赖用户手工配条件提醒，交易时段每 15min 自动盯
// 「已建仓的」和「重点关注的」两类系统性事件——止损/止盈触达、异常波动。
//   - 与条件提醒的边界：目标价类诉求仍走 AlertRule（用户显式配置）；守护只管这两类。
//   - 推送前置与所有通道一致：EnableNotify 总闸 + HasEnabledChannel（无启用通道的用户
//     直接跳过评估，省掉行情请求——守护是纯推送，不落任何页面可见数据，只有 GuardEvent 去重台账）。
//   - 财报类提醒的教训（alert.go:44-46）：本 job 只做批量行情评估，绝不在循环里逐 symbol
//     重接口调用——单轮单用户一次 QuotesFor 批量拉取。
//
// 持仓盘后事件轮（本批扩展）：每日 19:35 推持仓股的公告/龙虎榜上榜/财报披露临近/新业绩预告。
//   - 数据全部来自本地缓存表（announcements/lhb_entries/disclosure_schedules/earnings_forecasts），
//     零上游成本；19:35 错峰在 18:45 龙虎榜 job 与 19:05 财报+公告 job 之后，数据已就绪。
//   - 只覆盖持仓（用户最关心的「变化」），自选不做——公告推送面太宽易疲劳。
//   - 每天都跑（不判交易日）：公告周末也发布，龙虎榜非交易日自然无数据。
//   - 幂等靠 GuardEvent 台账：盘后事件的 trade_date 存事件自身日期（公告发布日/上榜日/
//     预约披露日），窗口重复扫描不重推；服务缺勤后下轮补推（迟到不丢）。
//   - 与 earn_date/earn_fcst AlertRule 的边界：AlertRule 是用户对单只股的显式订阅（自选
//     场景），本轮对持仓自动生效无需配置；两者都配时可能各推一条，可接受。

const (
	guardWindowStartMin = 9*60 + 30 // 09:30 开盘
	guardWindowEndMin   = 15*60 + 5 // 15:05（收盘后 5min 缓冲，覆盖尾盘触达）

	guardTickMinutes = 15 // 评估轮次间隔（对齐 alert job 节奏）

	guardEveningHour = 19 // 盘后事件轮 19:35（错峰：18:45 龙虎榜、19:05 财报+公告之后）
	guardEveningMin  = 35

	guardEventWindowDays = 2  // 盘后事件回看自然日窗口（today-2 起），服务缺勤时补推
	guardEarnAheadDays   = 3  // 财报披露临近提前量（自然日，对齐 earn_date 提醒默认）
	guardEveningPushCap  = 10 // 单用户单轮盘后推送上限（防首轮冷启动存量事件刷屏；超出只落台账）
)

// GuardService 守护推送评估与调度。
type GuardService struct {
	market *MarketService
	notify *NotifyService
}

func NewGuardService(market *MarketService) *GuardService {
	return &GuardService{market: market, notify: NewNotifyService()}
}

// ---------- 配置 ----------

// guardConfig 智能守护配置（UserPreference.GuardConfigJSON 的结构）。
// 空串 = 默认全开（parseGuardConfig 给默认值，风格对齐 RecFilters）。
type guardConfig struct {
	Enabled    bool    `json:"enabled"`     // 守护总开关
	PosPct     float64 `json:"pos_pct"`     // 持仓当日异动阈值 %（|涨跌幅| ≥ 此值推送）
	WatchPct   float64 `json:"watch_pct"`   // 守护范围自选当日异动阈值 %
	StopLoss   bool    `json:"stop_loss"`   // 持仓止损触达子开关
	TakeProfit bool    `json:"take_profit"` // 持仓止盈触达子开关
	Evening    bool    `json:"evening"`     // 持仓盘后事件子开关（公告/龙虎榜/财报披露/业绩预告）
}

// defaultGuardConfig 默认全开：pos±5%、watch±7%、止损止盈与盘后事件均开。
func defaultGuardConfig() guardConfig {
	return guardConfig{Enabled: true, PosPct: 5, WatchPct: 7, StopLoss: true, TakeProfit: true, Evening: true}
}

// sanitizeGuardConfig 归一化阈值：非正回退默认、上限钳制 30%（防误配空推或极端阈值）。
func sanitizeGuardConfig(c guardConfig) guardConfig {
	if c.PosPct <= 0 {
		c.PosPct = 5
	} else if c.PosPct > 30 {
		c.PosPct = 30
	}
	if c.WatchPct <= 0 {
		c.WatchPct = 7
	} else if c.WatchPct > 30 {
		c.WatchPct = 30
	}
	return c
}

// parseGuardConfig 解析存储的配置 JSON。空串或坏格式回退默认（全开）；
// 反序列化基于默认值填充——缺失布尔字段保持默认 true（子开关不因半份 JSON 被静默关闭）。
func parseGuardConfig(raw string) guardConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultGuardConfig()
	}
	c := defaultGuardConfig()
	if json.Unmarshal([]byte(raw), &c) != nil {
		return defaultGuardConfig()
	}
	return sanitizeGuardConfig(c)
}

// loadGuardConfig 读取用户偏好里的守护配置。
func loadGuardConfig(userID int64) guardConfig {
	var pref model.UserPreference
	if err := common.DB.Select("guard_config_json").Where("user_id = ?", userID).First(&pref).Error; err != nil {
		return defaultGuardConfig()
	}
	return parseGuardConfig(pref.GuardConfigJSON)
}

// ---------- 纯函数评估（单测锚点） ----------

// guardObs 单个标的的守护观测值（当日行情快照）。
type guardObs struct {
	Price     float64
	DayHigh   float64
	DayLow    float64
	ChangePct float64
}

// guardHit 一条命中的守护事件（待去重落库 + 推送）。
type guardHit struct {
	Symbol   string
	Market   string
	Name     string
	Kind     string
	Price    float64
	Message  string
	Priority int    // ntfy 优先级；止损/持仓跌停=4，其余=0（默认）
	Route    string // 点击跳转路由
	EventDate string // 盘后事件自身日期（去重台账 trade_date）；空=盘中事件，用评估当日
}

// evalPositionGuard 评估持仓的止损/止盈触达与当日异动，返回命中列表（可多条并存）。
// 止损/止盈的触达判定用当日 low/high 兜底——对齐 alert.go evaluateAlert 的 price 分支
//（盘中最低触及止损、最高触及止盈即算命中，不漏判盘中触达）。
// limitPct 为该标的板块涨跌停幅度（limitUpPctFor）：|涨跌幅| 接近该幅度（-0.3 容差对齐
// isAtLimitUp）即视为涨跌停——即使用户异动阈值配得比涨停幅还高也推（对齐 watch 侧先例）；
// 跌停对持仓是紧急事件，优先级与止损同为 4。
func evalPositionGuard(pos model.Position, cfg guardConfig, obs guardObs, limitPct float64) []guardHit {
	if obs.Price <= 0 {
		return nil
	}
	name := pos.Name
	if name == "" {
		name = pos.Symbol
	}
	var hits []guardHit

	if cfg.StopLoss && pos.PlanStopLoss > 0 {
		lo := obs.DayLow
		if lo <= 0 {
			lo = obs.Price
		}
		if lo <= pos.PlanStopLoss {
			hits = append(hits, guardHit{
				Symbol: pos.Symbol, Market: pos.Market, Name: name, Kind: model.GuardKindStopLoss,
				Price: obs.Price, Priority: 4, Route: "/positions",
				Message: fmt.Sprintf("⚠️ %s(%s) 触及计划止损 %s（现价 %.2f，当日最低 %.2f）",
					name, pos.Symbol, trimFloat(pos.PlanStopLoss), obs.Price, lo),
			})
		}
	}
	if cfg.TakeProfit && pos.PlanTakeProfit > 0 {
		hi := obs.DayHigh
		if hi <= 0 {
			hi = obs.Price
		}
		if hi >= pos.PlanTakeProfit {
			hits = append(hits, guardHit{
				Symbol: pos.Symbol, Market: pos.Market, Name: name, Kind: model.GuardKindTakeProfit,
				Price: obs.Price, Priority: 0, Route: "/positions",
				Message: fmt.Sprintf("🎯 %s(%s) 触及计划止盈 %s（现价 %.2f，当日最高 %.2f）",
					name, pos.Symbol, trimFloat(pos.PlanTakeProfit), obs.Price, hi),
			})
		}
	}
	if cfg.PosPct > 0 {
		abs := math.Abs(obs.ChangePct)
		limitReached := limitPct > 0 && abs >= limitPct-0.3
		if abs >= cfg.PosPct || limitReached {
			arrow, label, pri := "📈", "持仓异动", 0
			if obs.ChangePct < 0 {
				arrow = "📉"
			}
			if limitReached {
				if obs.ChangePct >= 0 {
					label = "持仓涨停"
				} else {
					label, pri = "持仓跌停", 4
				}
			}
			hits = append(hits, guardHit{
				Symbol: pos.Symbol, Market: pos.Market, Name: name, Kind: model.GuardKindPosMove,
				Price: obs.Price, Priority: pri, Route: stockDetailRoute(pos.Market, pos.Symbol),
				Message: fmt.Sprintf("%s %s：%s(%s) 当日 %+.2f%%（现价 %.2f）",
					arrow, label, name, pos.Symbol, obs.ChangePct, obs.Price),
			})
		}
	}
	return hits
}

// evalWatchGuard 评估守护范围自选的当日异动或涨跌停，命中返回一条事件（否则 nil）。
// limitPct 为该标的板块涨跌停幅度（limitUpPctFor）：|涨跌幅| 接近该幅度即视为涨跌停
//（-0.3 容差对齐 isAtLimitUp）；用户阈值 watch_pct 未达但触及涨跌停也推。
func evalWatchGuard(item model.WatchlistItem, cfg guardConfig, obs guardObs, limitPct float64) *guardHit {
	if obs.Price <= 0 || cfg.WatchPct <= 0 {
		return nil
	}
	abs := math.Abs(obs.ChangePct)
	limitReached := limitPct > 0 && abs >= limitPct-0.3
	if abs < cfg.WatchPct && !limitReached {
		return nil
	}
	name := item.Name
	if name == "" {
		name = item.Symbol
	}
	arrow := "📈"
	label := "自选异动"
	if obs.ChangePct < 0 {
		arrow = "📉"
	}
	if limitReached {
		if obs.ChangePct >= 0 {
			label = "自选涨停"
		} else {
			label = "自选跌停"
		}
	}
	return &guardHit{
		Symbol: item.Symbol, Market: item.Market, Name: name, Kind: model.GuardKindWatchMove,
		Price: obs.Price, Priority: 0, Route: stockDetailRoute(item.Market, item.Symbol),
		Message: fmt.Sprintf("%s %s：%s(%s) 当日 %+.2f%%（现价 %.2f）",
			arrow, label, name, item.Symbol, obs.ChangePct, obs.Price),
	}
}

// stockDetailRoute 个股详情前端路由（与 web 路由 /stocks/:market/:symbol 一致）。
func stockDetailRoute(market, symbol string) string {
	return "/stocks/" + market + "/" + symbol
}

// inGuardWindow 是否处于守护评估时段（周一~五 09:30~15:05，本地时区）。纯函数可测。
// 交易日历（节假日）判定另由 isTradingDayToday 兜底，二者在 runGuardRound 中并用。
func inGuardWindow(t time.Time) bool {
	if wd := t.Weekday(); wd < time.Monday || wd > time.Friday {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= guardWindowStartMin && m < guardWindowEndMin
}

// guardTitle 推送标题（简短类别，正文在 Content）。
func guardTitle(kind string) string {
	switch kind {
	case model.GuardKindStopLoss:
		return "QuantVista 止损提醒"
	case model.GuardKindTakeProfit:
		return "QuantVista 止盈提醒"
	case model.GuardKindPosMove:
		return "QuantVista 持仓异动"
	case model.GuardKindWatchMove:
		return "QuantVista 自选异动"
	case model.GuardKindPosNotice:
		return "QuantVista 持仓公告"
	case model.GuardKindPosLhb:
		return "QuantVista 持仓龙虎榜"
	case model.GuardKindPosEarnDate:
		return "QuantVista 持仓财报提醒"
	case model.GuardKindPosEarnFcst:
		return "QuantVista 持仓业绩预告"
	}
	return "QuantVista 守护提醒"
}

// ---------- 台账去重 ----------

// recordGuardEvent 落库守护事件，唯一索引冲突时跳过。返回是否为「本轮新事件」
//（RowsAffected>0）——同日同标的同类事件只在首次命中返回 true，用于驱动推送。
func recordGuardEvent(userID int64, tradeDate string, h guardHit) bool {
	ev := model.GuardEvent{
		UserID: userID, Symbol: h.Symbol, Kind: h.Kind, TradeDate: tradeDate,
		Market: h.Market, Name: h.Name, Price: round4(h.Price), Message: truncateRunes(h.Message, 256),
	}
	res := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&ev)
	return res.Error == nil && res.RowsAffected > 0
}

// ---------- 评估编排 ----------

// guardCandidateUserIDs 有守护对象的用户（持仓 holding ∪ 守护范围自选），去重。
// 守护范围自选：IsPinned=true 或 ResearchStage ∈ {waiting_price, planned}——普通自选不推，防疲劳。
func guardCandidateUserIDs() []int64 {
	set := map[int64]bool{}
	var ids []int64
	common.DB.Model(&model.Position{}).
		Where("status = ? AND market = ?", model.PositionStatusHolding, "cn").
		Distinct().Pluck("user_id", &ids)
	for _, id := range ids {
		set[id] = true
	}
	var wids []int64
	common.DB.Model(&model.WatchlistItem{}).
		Where("market = ? AND (is_pinned = ? OR research_stage IN ?)",
			"cn", true, []string{model.StageWaitingPrice, model.StagePlanned}).
		Distinct().Pluck("user_id", &wids)
	for _, id := range wids {
		set[id] = true
	}
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// evaluateGuardUser 评估单个用户的持仓与守护自选，落库新事件并推送。返回新事件数。
// 同一 symbol 同为持仓与自选时，持仓侧已评估（pos_move），自选侧跳过避免重复推。
func (s *GuardService) evaluateGuardUser(ctx context.Context, userID int64, cfg guardConfig, tradeDate string) int {
	var positions []model.Position
	common.DB.Where("user_id = ? AND status = ? AND market = ?",
		userID, model.PositionStatusHolding, "cn").Find(&positions)
	var items []model.WatchlistItem
	common.DB.Where("user_id = ? AND market = ? AND (is_pinned = ? OR research_stage IN ?)",
		userID, "cn", true, []string{model.StageWaitingPrice, model.StagePlanned}).Find(&items)
	if len(positions) == 0 && len(items) == 0 {
		return 0
	}

	// 批量行情：去重 symbol 后一次 FreshQuotesFor（并发上限内部限流，绝不逐 symbol 重调）。
	// fail-closed：stale 行情不参与守护评估——旧价触发止损推送是误报（昨日击穿今日已回升），
	// 该标的本轮跳过，下一轮行情恢复再评。
	held := map[string]bool{}
	seen := map[string]bool{}
	var refs []QuoteRef
	addRef := func(market, symbol string) {
		k := QuoteKey(market, symbol)
		if seen[k] {
			return
		}
		seen[k] = true
		refs = append(refs, QuoteRef{Market: market, Symbol: symbol})
	}
	for _, p := range positions {
		held[p.Symbol] = true
		addRef(p.Market, p.Symbol)
	}
	for _, it := range items {
		if held[it.Symbol] {
			continue
		}
		addRef(it.Market, it.Symbol)
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)
	freshQuote := func(market, symbol string) *datasource.Quote {
		fq, ok := quotes[QuoteKey(market, symbol)]
		// fail-closed：仅 fresh 放行（unknown 同样不评——无法核验时效的行情驱动
		// 守护推送与旧价误触发同责）。
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			return nil
		}
		return fq.Quote
	}

	var newHits []guardHit
	for _, p := range positions {
		q := freshQuote(p.Market, p.Symbol)
		if q == nil {
			continue
		}
		obs := guardObs{Price: q.Price, DayHigh: q.High, DayLow: q.Low, ChangePct: q.ChangePct}
		for _, h := range evalPositionGuard(p, cfg, obs, limitUpPctFor(p.Symbol, p.Name)) {
			if recordGuardEvent(userID, tradeDate, h) {
				newHits = append(newHits, h)
			}
		}
	}
	for _, it := range items {
		if held[it.Symbol] {
			continue // 持仓侧已覆盖，不重复自选异动
		}
		q := freshQuote(it.Market, it.Symbol)
		if q == nil {
			continue
		}
		obs := guardObs{Price: q.Price, DayHigh: q.High, DayLow: q.Low, ChangePct: q.ChangePct}
		if h := evalWatchGuard(it, cfg, obs, limitUpPctFor(it.Symbol, it.Name)); h != nil {
			if recordGuardEvent(userID, tradeDate, *h) {
				newHits = append(newHits, *h)
			}
		}
	}

	// 逐条推送新事件（各带自身 Route/Priority，无法聚合成单条；同日去重已保证不刷屏）。
	for _, h := range newHits {
		s.notify.SendMsg(userID, NotifyMessage{
			Title: guardTitle(h.Kind), Content: h.Message,
			Route: h.Route, Kind: NotifyMsgKindGuard, Priority: h.Priority,
		})
	}
	return len(newHits)
}

// ---------- 盘后持仓事件（公告/龙虎榜/财报披露/业绩预告，纯本地表查询） ----------

// evalPosAnnouncements 持仓公告：窗口内公告按发布日分组，每组聚合一条事件
//（同股同日多条公告合并成一条推送；去重键 trade_date=发布日，跨轮幂等）。
func evalPosAnnouncements(symbol, name string, anns []model.Announcement) []guardHit {
	if len(anns) == 0 {
		return nil
	}
	byDate := map[string][]model.Announcement{}
	var dates []string
	for _, a := range anns {
		if _, ok := byDate[a.NoticeDate]; !ok {
			dates = append(dates, a.NoticeDate)
		}
		byDate[a.NoticeDate] = append(byDate[a.NoticeDate], a)
	}
	var hits []guardHit
	for _, d := range dates {
		group := byDate[d]
		if name == "" {
			name = group[0].Name
		}
		if name == "" {
			name = symbol
		}
		titles := make([]string, 0, len(group))
		for _, a := range group {
			titles = append(titles, "《"+a.Title+"》")
		}
		hits = append(hits, guardHit{
			Symbol: symbol, Market: "cn", Name: name, Kind: model.GuardKindPosNotice,
			Route: stockDetailRoute("cn", symbol), EventDate: d,
			Message: truncateRunes(fmt.Sprintf("📄 持仓公告：%s(%s) %s 发布 %d 条：%s",
				name, symbol, d, len(group), strings.Join(titles, "；")), 256),
		})
	}
	return hits
}

// evalPosLhb 持仓登龙虎榜：窗口内榜单按交易日分组（同股同日多上榜原因合并一条）。
func evalPosLhb(symbol, name string, entries []model.LhbEntry) []guardHit {
	if len(entries) == 0 {
		return nil
	}
	byDate := map[string][]model.LhbEntry{}
	var dates []string
	for _, e := range entries {
		if _, ok := byDate[e.TradeDate]; !ok {
			dates = append(dates, e.TradeDate)
		}
		byDate[e.TradeDate] = append(byDate[e.TradeDate], e)
	}
	var hits []guardHit
	for _, d := range dates {
		group := byDate[d]
		if name == "" {
			name = group[0].Name
		}
		if name == "" {
			name = symbol
		}
		reasons := make([]string, 0, len(group))
		seen := map[string]bool{}
		for _, e := range group {
			if e.Reason != "" && !seen[e.Reason] {
				seen[e.Reason] = true
				reasons = append(reasons, e.Reason)
			}
		}
		reasonTxt := ""
		if len(reasons) > 0 {
			reasonTxt = "（" + strings.Join(reasons, "；") + "）"
		}
		hits = append(hits, guardHit{
			Symbol: symbol, Market: "cn", Name: name, Kind: model.GuardKindPosLhb,
			Price: group[0].Close, Route: stockDetailRoute("cn", symbol), EventDate: d,
			Message: truncateRunes(fmt.Sprintf("🐉 持仓上榜：%s(%s) %s 登龙虎榜%s，榜内净买 %+.2f 亿",
				name, symbol, d, reasonTxt, group[0].NetBuy/1e8), 256),
		})
	}
	return hits
}

// evalPosEarnDate 持仓财报披露临近：预约披露日在 [today, today+N] 内（N=guardEarnAheadDays）。
// 去重键 trade_date=预约披露日——整个临近窗口只推一次；披露日变更（新 AppointDate）会再推。
func evalPosEarnDate(symbol, name string, sched *model.DisclosureSchedule, today string) *guardHit {
	if sched == nil || sched.AppointDate == "" {
		return nil
	}
	ad, err := time.ParseInLocation("2006-01-02", sched.AppointDate, time.Local)
	if err != nil {
		return nil
	}
	td, err := time.ParseInLocation("2006-01-02", today, time.Local)
	if err != nil {
		return nil
	}
	days := int(ad.Sub(td).Hours() / 24)
	if days < 0 || days > guardEarnAheadDays {
		return nil
	}
	if name == "" {
		name = sched.Name
	}
	if name == "" {
		name = symbol
	}
	label := sched.ReportTypeName
	if label == "" {
		label = "财报"
	}
	when := fmt.Sprintf("%d 天后", days)
	if days == 0 {
		when = "今日"
	}
	return &guardHit{
		Symbol: symbol, Market: "cn", Name: name, Kind: model.GuardKindPosEarnDate,
		Route: stockDetailRoute("cn", symbol), EventDate: sched.AppointDate,
		Message: fmt.Sprintf("📅 持仓财报：%s(%s) 将于 %s 披露%s（%s），留意业绩波动",
			name, symbol, sched.AppointDate, label, when),
	}
}

// evalPosEarnFcst 持仓新业绩预告：发布日落在回看窗口内即推（去重键 trade_date=发布日）。
func evalPosEarnFcst(symbol, name string, fc *model.EarningsForecast, since string) *guardHit {
	if fc == nil || fc.NoticeDate == "" || fc.NoticeDate < since {
		return nil
	}
	if name == "" {
		name = fc.Name
	}
	if name == "" {
		name = symbol
	}
	typ := fc.PredictType
	if typ == "" {
		typ = "业绩预告"
	}
	detail := ""
	switch {
	case fc.AmpLower != 0 || fc.AmpUpper != 0:
		detail = fmt.Sprintf("，%s变动 %.2f%%~%.2f%%", fc.PredictFinance, fc.AmpLower, fc.AmpUpper)
	case fc.Content != "":
		detail = "：" + truncateRunes(fc.Content, 60)
	}
	return &guardHit{
		Symbol: symbol, Market: "cn", Name: name, Kind: model.GuardKindPosEarnFcst,
		Route: stockDetailRoute("cn", symbol), EventDate: fc.NoticeDate,
		Message: truncateRunes(fmt.Sprintf("📊 持仓业绩预告：%s(%s) %s 发布【%s】%s",
			name, symbol, fc.NoticeDate, typ, detail), 256),
	}
}

// evaluateGuardUserEvening 单用户盘后事件评估：持仓 symbol 集合 → 四类本地表批量查询 →
// 纯函数评估 → 台账去重 → 推送（cap 限流防冷启动存量刷屏）。返回新事件数。
func (s *GuardService) evaluateGuardUserEvening(userID int64, today, since string) int {
	var positions []model.Position
	common.DB.Where("user_id = ? AND status = ? AND market = ?",
		userID, model.PositionStatusHolding, "cn").Find(&positions)
	if len(positions) == 0 {
		return 0
	}
	// 同 symbol 多笔持仓只评一次（盘后事件与持仓行的止损价等无关，只看标的）。
	nameBySym := map[string]string{}
	var syms []string
	for _, p := range positions {
		if _, ok := nameBySym[p.Symbol]; !ok {
			syms = append(syms, p.Symbol)
			nameBySym[p.Symbol] = p.Name
		}
	}

	// 四类数据一次性批量查询（symbol IN），绝不逐 symbol 循环查。
	var anns []model.Announcement
	common.DB.Where("symbol IN ? AND notice_date >= ?", syms, since).
		Order("notice_date, id").Find(&anns)
	var lhbs []model.LhbEntry
	common.DB.Where("symbol IN ? AND trade_date >= ?", syms, since).
		Order("trade_date, id").Find(&lhbs)
	untilDate := mustAddDays(today, guardEarnAheadDays)
	var scheds []model.DisclosureSchedule
	common.DB.Where("symbol IN ? AND is_published = ? AND appoint_date BETWEEN ? AND ?",
		syms, false, today, untilDate).Order("appoint_date").Find(&scheds)
	var fcs []model.EarningsForecast
	common.DB.Where("symbol IN ? AND notice_date >= ?", syms, since).
		Order("notice_date DESC, id DESC").Find(&fcs)

	annBySym := map[string][]model.Announcement{}
	for _, a := range anns {
		annBySym[a.Symbol] = append(annBySym[a.Symbol], a)
	}
	lhbBySym := map[string][]model.LhbEntry{}
	for _, e := range lhbs {
		lhbBySym[e.Symbol] = append(lhbBySym[e.Symbol], e)
	}
	schedBySym := map[string]*model.DisclosureSchedule{} // 每 symbol 最近一期（已按 appoint_date 升序）
	for i := range scheds {
		if _, ok := schedBySym[scheds[i].Symbol]; !ok {
			schedBySym[scheds[i].Symbol] = &scheds[i]
		}
	}
	fcBySym := map[string]*model.EarningsForecast{} // 每 symbol 最新一份预告（已按 notice_date 降序）
	for i := range fcs {
		if _, ok := fcBySym[fcs[i].Symbol]; !ok {
			fcBySym[fcs[i].Symbol] = &fcs[i]
		}
	}

	var newHits []guardHit
	for _, sym := range syms {
		name := nameBySym[sym]
		var hits []guardHit
		hits = append(hits, evalPosAnnouncements(sym, name, annBySym[sym])...)
		hits = append(hits, evalPosLhb(sym, name, lhbBySym[sym])...)
		if h := evalPosEarnDate(sym, name, schedBySym[sym], today); h != nil {
			hits = append(hits, *h)
		}
		if h := evalPosEarnFcst(sym, name, fcBySym[sym], since); h != nil {
			hits = append(hits, *h)
		}
		for _, h := range hits {
			date := h.EventDate
			if date == "" {
				date = today // 兜底：事件日期缺失按评估日去重（正常路径四类纯函数都必填）
			}
			if recordGuardEvent(userID, date, h) {
				newHits = append(newHits, h)
			}
		}
	}

	// 推送上限：首轮部署/首次开启时窗口内存量事件可能很多，超出只落台账不推
	//（台账已记，不会顺延到下轮——存量旧闻主动放弃，日常增量远达不到上限）。
	pushed := 0
	for _, h := range newHits {
		if pushed >= guardEveningPushCap {
			common.SysLog("用户 %d 盘后守护事件超推送上限，%d 条只落台账", userID, len(newHits)-pushed)
			break
		}
		s.notify.SendMsg(userID, NotifyMessage{
			Title: guardTitle(h.Kind), Content: h.Message,
			Route: h.Route, Kind: NotifyMsgKindGuard, Priority: h.Priority,
		})
		pushed++
	}
	return len(newHits)
}

// runGuardEveningRound 盘后事件轮：每天 19:35（含周末——公告周末也发布；龙虎榜等
// 非交易日自然无数据）。数据依赖 18:45 龙虎榜 job 与 19:05 财报+公告 job 已跑完。
func (s *GuardService) runGuardEveningRound() {
	if common.DB == nil {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	since := now.AddDate(0, 0, -guardEventWindowDays).Format("2006-01-02")
	var ids []int64
	common.DB.Model(&model.Position{}).
		Where("status = ? AND market = ?", model.PositionStatusHolding, "cn").
		Distinct().Pluck("user_id", &ids)
	for _, uid := range ids {
		if !userNotifyEnabled(uid) || !s.notify.HasEnabledChannel(uid) {
			continue
		}
		cfg := loadGuardConfig(uid)
		if !cfg.Enabled || !cfg.Evening {
			continue
		}
		if n := s.evaluateGuardUserEvening(uid, today, since); n > 0 {
			common.SysLog("用户 %d 盘后守护事件 %d 条", uid, n)
		}
	}
}

// mustAddDays 日期字符串加 N 自然日（解析失败返回原串，调用方查询条件退化为当日）。
func mustAddDays(date string, days int) string {
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// runGuardRound 一轮评估：非交易时段/非交易日直接返回；否则遍历有推送意愿的候选用户。
func (s *GuardService) runGuardRound() {
	if common.DB == nil {
		return
	}
	now := time.Now()
	if !inGuardWindow(now) || !isTradingDayToday(now) {
		return
	}
	tradeDate := now.Format("2006-01-02")
	for _, uid := range guardCandidateUserIDs() {
		// 推送前置：总闸开 + 有启用通道。守护是纯推送，无通道的用户跳过评估省行情请求。
		if !userNotifyEnabled(uid) || !s.notify.HasEnabledChannel(uid) {
			continue
		}
		cfg := loadGuardConfig(uid)
		if !cfg.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		n := s.evaluateGuardUser(ctx, uid, cfg, tradeDate)
		cancel()
		if n > 0 {
			common.SysLog("用户 %d 守护推送 %d 条", uid, n)
		}
	}
}

// StartGuardJobs 后台守护评估：
//   - 盘中轮：启动延迟 75s（错开 alert 的 60s 启动）后首评，此后每 15 分钟一轮
//    （runGuardRound 内部自判交易时段，非盘中空转即返回）；
//   - 盘后事件轮：每天 19:35（数据依赖 18:45/19:05 两个 job 已错峰跑完）；启动 3 分钟后
//     先补跑一轮（服务缺勤后补推窗口内漏掉的事件，GuardEvent 台账保证不重复）。
func StartGuardJobs(mgr *datasource.Manager) *GuardService {
	svc := NewGuardService(NewMarketService(mgr))
	go func() {
		time.Sleep(75 * time.Second)
		svc.runGuardRound()
		t := time.NewTicker(guardTickMinutes * time.Minute)
		defer t.Stop()
		for range t.C {
			svc.runGuardRound()
		}
	}()
	go func() {
		time.Sleep(3 * time.Minute)
		svc.runGuardEveningRound()
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), guardEveningHour, guardEveningMin, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			svc.runGuardEveningRound()
		}
	}()
	return svc
}
