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

const (
	guardWindowStartMin = 9*60 + 30 // 09:30 开盘
	guardWindowEndMin   = 15*60 + 5 // 15:05（收盘后 5min 缓冲，覆盖尾盘触达）

	guardTickMinutes = 15 // 评估轮次间隔（对齐 alert job 节奏）
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
}

// defaultGuardConfig 默认全开：pos±5%、watch±7%、止损止盈均开。
func defaultGuardConfig() guardConfig {
	return guardConfig{Enabled: true, PosPct: 5, WatchPct: 7, StopLoss: true, TakeProfit: true}
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
	Priority int    // ntfy 优先级；止损=4，其余=0（默认）
	Route    string // 点击跳转路由
}

// evalPositionGuard 评估持仓的止损/止盈触达与当日异动，返回命中列表（可多条并存）。
// 止损/止盈的触达判定用当日 low/high 兜底——对齐 alert.go evaluateAlert 的 price 分支
//（盘中最低触及止损、最高触及止盈即算命中，不漏判盘中触达）。
func evalPositionGuard(pos model.Position, cfg guardConfig, obs guardObs) []guardHit {
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
	if cfg.PosPct > 0 && math.Abs(obs.ChangePct) >= cfg.PosPct {
		arrow := "📈"
		if obs.ChangePct < 0 {
			arrow = "📉"
		}
		hits = append(hits, guardHit{
			Symbol: pos.Symbol, Market: pos.Market, Name: name, Kind: model.GuardKindPosMove,
			Price: obs.Price, Priority: 0, Route: stockDetailRoute(pos.Market, pos.Symbol),
			Message: fmt.Sprintf("%s 持仓异动：%s(%s) 当日 %+.2f%%（现价 %.2f）",
				arrow, name, pos.Symbol, obs.ChangePct, obs.Price),
		})
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

	// 批量行情：去重 symbol 后一次 QuotesFor（并发上限内部限流，绝不逐 symbol 重调）。
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
	quotes := s.market.QuotesFor(ctx, refs)

	var newHits []guardHit
	for _, p := range positions {
		q := quotes[QuoteKey(p.Market, p.Symbol)]
		if q == nil || q.Price <= 0 {
			continue
		}
		obs := guardObs{Price: q.Price, DayHigh: q.High, DayLow: q.Low, ChangePct: q.ChangePct}
		for _, h := range evalPositionGuard(p, cfg, obs) {
			if recordGuardEvent(userID, tradeDate, h) {
				newHits = append(newHits, h)
			}
		}
	}
	for _, it := range items {
		if held[it.Symbol] {
			continue // 持仓侧已覆盖，不重复自选异动
		}
		q := quotes[QuoteKey(it.Market, it.Symbol)]
		if q == nil || q.Price <= 0 {
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

// StartGuardJobs 后台守护评估：启动延迟 75s（错开 alert 的 60s 启动）后首评，
// 此后每 15 分钟一轮（runGuardRound 内部自判交易时段，非盘中空转即返回）。
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
	return svc
}
