package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 收盘日报：交易日 15:35 后为开启偏好的用户自动生成「今日复盘 + 明日选股推荐」。
// 复盘 = 聚合市场/持仓/自选/提醒数据 → LLM 结构化总结（快照落库可复现）；
// 推荐 = 复用 RecommendationService（短线，含买点区间/止盈/止损/有效期），
// 并为每条推荐自动创建到价卖点提醒（note 前缀「收盘日报」，过期自动暂停）。
// 后台自动生成不消耗次数配额（token 照记审计）；手动重生成计 1 次。

const (
	reportWindowStartMin = 15*60 + 35 // 15:35，收盘数据已稳定
	reportWindowEndMin   = 20 * 60    // 20:00 后不再补生成
	reportAlertNotePrefix = "收盘日报"
	reportAlertExpireDays = 21 // 卖点提醒规则超过该自然日数自动暂停（覆盖短线最长有效期）
)

// DailyReportService 依赖既有服务拼装，不重复造数据链路。
type DailyReportService struct {
	market    *MarketService
	watchlist *WatchlistService
	position  *PositionService
	alert     *AlertService
	rec       *RecommendationService
	llm       *LLMService
	notify    *NotifyService
}

func NewDailyReportService(market *MarketService, watchlist *WatchlistService, position *PositionService,
	alert *AlertService, rec *RecommendationService, llm *LLMService, notify *NotifyService) *DailyReportService {
	return &DailyReportService{market: market, watchlist: watchlist, position: position,
		alert: alert, rec: rec, llm: llm, notify: notify}
}

// dailyReview LLM 复盘的结构化输出。
type dailyReview struct {
	Summary        string   `json:"summary"`
	MarketReview   string   `json:"market_review"`
	PositionReview string   `json:"position_review"`
	WatchReview    string   `json:"watch_review"`
	RiskWarnings   []string `json:"risk_warnings"`
	TomorrowPlan   string   `json:"tomorrow_plan"`

	// 服务端回填（非 LLM 输出）：复盘文本引用数字与快照值域的程序化核验。
	EvidenceCheck *evidenceCheck `json:"evidence_check,omitempty"`
}

// inReportWindow 是否处于日报生成窗口（收盘后 15:35 ~ 20:00，本地时区）。纯函数可测。
func inReportWindow(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	return m >= reportWindowStartMin && m < reportWindowEndMin
}

// isTradingDayToday cn 市场今天是否交易日：优先查交易日历；无日历数据时回退「周一~五」。
func isTradingDayToday(now time.Time) bool {
	date := now.Format("2006-01-02")
	var cal model.TradingCalendar
	err := common.DB.Where("market = ? AND trade_date = ?", "cn", date).First(&cal).Error
	if err == nil {
		return cal.IsOpen
	}
	wd := now.Weekday()
	return wd >= time.Monday && wd <= time.Friday
}

// List 用户的日报列表（排除大字段）。
func (s *DailyReportService) List(userID int64, limit int) ([]model.DailyReport, error) {
	if limit <= 0 || limit > 60 {
		limit = 20
	}
	var rows []model.DailyReport
	err := common.DB.Select("id", "user_id", "trade_date", "market", "status",
		"recommendation_batch_id", "error", "total_tokens", "latency_ms", "created_at", "updated_at").
		Where("user_id = ?", userID).Order("trade_date DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Get 日报详情（含复盘全文与推荐批次视图）。
func (s *DailyReportService) Get(userID, id int64) (*DailyReportView, error) {
	var r model.DailyReport
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&r).Error; err != nil {
		return nil, errors.New("日报不存在")
	}
	return s.assembleView(&r), nil
}

// Latest 最新一份日报（首页「AI 今日观点」卡用）。无日报返回 nil 不报错。
func (s *DailyReportService) Latest(userID int64) (*DailyReportView, error) {
	var r model.DailyReport
	err := common.DB.Where("user_id = ?", userID).Order("trade_date DESC, id DESC").First(&r).Error
	if err != nil {
		return nil, nil
	}
	return s.assembleView(&r), nil
}

// DailyReportView 详情视图：复盘解析 + 推荐批次（复用推荐域视图）。
type DailyReportView struct {
	model.DailyReport
	Review         *dailyReview        `json:"review"`
	Recommendation *RecommendationView `json:"recommendation"`
}

func (s *DailyReportService) assembleView(r *model.DailyReport) *DailyReportView {
	v := &DailyReportView{DailyReport: *r}
	if r.ReviewJSON != "" {
		var rv dailyReview
		if json.Unmarshal([]byte(r.ReviewJSON), &rv) == nil {
			v.Review = &rv
		}
	}
	if r.RecommendationBatchID > 0 {
		if rec, err := s.rec.Get(r.UserID, r.RecommendationBatchID); err == nil {
			v.Recommendation = rec
		}
	}
	return v
}

// GenerateFor 为用户生成当日日报。manual=true（手动重生成）时：允许覆盖当日旧报告、
// 消耗 1 次配额；auto 时已存在即跳过、不计次。返回生成的报告视图。
func (s *DailyReportService) GenerateFor(ctx context.Context, userID int64, manual bool) (*DailyReportView, error) {
	now := time.Now()
	if !isTradingDayToday(now) {
		return nil, errors.New("今日休市，无日报可生成")
	}
	date := now.Format("2006-01-02")

	var existing model.DailyReport
	exists := common.DB.Where("user_id = ? AND trade_date = ?", userID, date).First(&existing).Error == nil
	if exists && !manual {
		return s.assembleView(&existing), nil
	}

	// LLM 配置解析 + 手动时的配额熔断（自动生成不占次数，但额度用尽也不再代烧 token）。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, 0)
	if err != nil {
		return nil, fmt.Errorf("未配置可用的 LLM：%w", err)
	}
	if err := checkQuota(userID); err != nil {
		return nil, err
	}
	allowPrivate := isAdminUser(userID)

	// 手动重生成：清掉当日旧报告与旧卖点提醒规则，避免重复。
	if exists && manual {
		common.DB.Delete(&model.DailyReport{}, existing.ID)
		common.DB.Where("user_id = ? AND note LIKE ?", userID, reportAlertNotePrefix+" "+date+"%").
			Delete(&model.AlertRule{})
	}

	report := &model.DailyReport{UserID: userID, TradeDate: date, Market: "cn"}
	start := time.Now()

	// 1) 复盘：聚合快照 → LLM 总结（1 次 repair）。
	snapshot := s.buildSnapshot(ctx, userID, date)
	snapJSON, _ := json.Marshal(snapshot)
	report.SnapshotJSON = string(snapJSON)

	review, reviewTokens, reviewErr := s.callReview(ctx, cfg, apiKey, allowPrivate, string(snapJSON))
	report.TotalTokens += reviewTokens
	if reviewErr == nil {
		review.EvidenceCheck = dailyReviewEvidence(review, snapshot) // 信任层回填后随 ReviewJSON 一起落库
		b, _ := json.Marshal(review)
		report.ReviewJSON = string(b)
	}
	if reviewTokens > 0 {
		consumeQuota(userID, reviewTokens, false) // 复盘 token 记审计；次数在末尾按 manual 记一次
	}

	// 2) 明日推荐：复用推荐域（短线：买点/止盈/止损/有效期）。失败不阻断复盘落库。
	// 策略按当日市场环境动态选择（旧版恒为第一个策略「动量突破」，弱市里追动量不合理）；
	// 筛选条件走用户偏好（Filters=nil 时推荐域自动加载）。
	pref := s.userPref(userID)
	recReq := RecommendRequest{
		Type: model.RecTypeShortTerm, Market: "cn", Count: pref.DefaultRecCount,
		Strategy: pickDailyStrategy(snapshot),
	}
	recView, recErr := s.rec.GenerateAuto(ctx, userID, allowPrivate, recReq)
	if recErr == nil && recView != nil {
		report.RecommendationBatchID = recView.ID
		// 3) 卖点提醒：每条推荐建 止盈(gte)/止损(lte) 到价规则，note 带日期便于过期清理。
		s.createSellAlerts(ctx, userID, date, recView)
	}

	// 状态归纳：双成 success / 单成 partial / 双败 failed。
	switch {
	case reviewErr == nil && recErr == nil:
		report.Status = model.ReportStatusSuccess
	case reviewErr != nil && recErr != nil:
		report.Status = model.ReportStatusFailed
	default:
		report.Status = model.ReportStatusPartial
	}
	var errParts []string
	if reviewErr != nil {
		errParts = append(errParts, "复盘失败: "+reviewErr.Error())
	}
	if recErr != nil {
		errParts = append(errParts, "推荐失败: "+recErr.Error())
	}
	report.Error = truncateRunes(strings.Join(errParts, "；"), 500)
	report.LatencyMs = time.Since(start).Milliseconds()

	if manual {
		chargeAction(userID) // 手动重生成计 1 次（token 已在各环节以 manual=false 记过审计）
	}
	if err := common.DB.Create(report).Error; err != nil {
		return nil, err
	}

	// 4) 推送摘要（best-effort，受推送通道与偏好总闸控制）。
	if review != nil && pref.EnableNotify && s.notify.HasEnabledChannel(userID) {
		go s.notify.Send(userID, fmt.Sprintf("收盘日报 %s", date), review.Summary)
	}
	return s.assembleView(report), nil
}

// ---- 数据聚合 ----

type reportSnapshot struct {
	TradeDate string            `json:"trade_date"`
	Market    *reportMarket     `json:"market,omitempty"`
	Positions []reportPosition  `json:"positions"`
	Watch     []reportWatchItem `json:"watch_movers"`
	Alerts    []string          `json:"alerts_today"`
	Note      string            `json:"note,omitempty"`
}

type reportMarket struct {
	Indices  []map[string]any `json:"indices,omitempty"`
	Breadth  map[string]any   `json:"breadth,omitempty"`
	FundFlow map[string]any   `json:"fund_flow,omitempty"`
}

type reportPosition struct {
	Symbol      string  `json:"symbol"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	ChangePct   float64 `json:"change_pct_today"`
	ProfitPct   float64 `json:"profit_pct_total"`
	NearStop    bool    `json:"near_stop_loss,omitempty"`
	BelowStop   bool    `json:"below_stop_loss,omitempty"`
	ReviewFlags string  `json:"review_flags,omitempty"`
}

type reportWatchItem struct {
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	ChangePct float64 `json:"change_pct"`
}

// buildSnapshot 聚合复盘输入：市场概览 + 持仓（含当日涨跌）+ 自选异动 top + 今日命中提醒。
// 单块失败降级为空，不阻断（快照里如实缺席，prompt 要求不编造）。
func (s *DailyReportService) buildSnapshot(ctx context.Context, userID int64, date string) *reportSnapshot {
	snap := &reportSnapshot{TradeDate: date, Positions: []reportPosition{}, Watch: []reportWatchItem{}, Alerts: []string{}}

	if ov := s.market.GetOverview(ctx, "cn"); ov != nil {
		m := &reportMarket{}
		for i, ix := range ov.Indices {
			if i >= 4 {
				break
			}
			m.Indices = append(m.Indices, map[string]any{"name": ix.Name, "price": ix.Price, "change_pct": ix.ChangePct})
		}
		if ov.Breadth != nil {
			m.Breadth = map[string]any{"advances": ov.Breadth.Advances, "declines": ov.Breadth.Declines,
				"limit_up": ov.Breadth.LimitUp, "limit_down": ov.Breadth.LimitDown}
		}
		if ov.FundFlow != nil {
			m.FundFlow = map[string]any{"main_net_yi": round2(ov.FundFlow.MainNet / 1e8)}
		}
		snap.Market = m
	}

	// 持仓：当日涨跌用批量行情富化（List 已含累计盈亏与止损标记）。
	if views, err := s.position.List(ctx, userID, "holding"); err == nil {
		refs := make([]QuoteRef, 0, len(views))
		for _, v := range views {
			refs = append(refs, QuoteRef{Market: v.Market, Symbol: v.Symbol})
		}
		quotes := s.market.QuotesFor(ctx, refs)
		for _, v := range views {
			p := reportPosition{Symbol: v.Symbol, Name: v.Name, Type: v.PositionType,
				ProfitPct: v.ProfitPct, NearStop: v.NearStopLoss, BelowStop: v.BelowStopLoss}
			if q := quotes[QuoteKey(v.Market, v.Symbol)]; q != nil {
				p.ChangePct = q.ChangePct
			}
			var flags []string
			if v.ShortTermReview {
				flags = append(flags, "短线持有超期建议复盘")
			}
			if v.BelowStopLoss {
				flags = append(flags, "已破计划止损")
			} else if v.NearStopLoss {
				flags = append(flags, "接近计划止损")
			}
			p.ReviewFlags = strings.Join(flags, "、")
			snap.Positions = append(snap.Positions, p)
		}
	}

	// 自选异动：全部分组条目按当日涨跌绝对值取前 8。
	if groups, err := s.watchlist.List(ctx, userID); err == nil {
		var items []reportWatchItem
		for _, g := range groups {
			for _, it := range g.Items {
				if it.QuoteOK {
					items = append(items, reportWatchItem{Symbol: it.Symbol, Name: it.Name, ChangePct: it.ChangePct})
				}
			}
		}
		for i := 0; i < len(items); i++ {
			for j := i + 1; j < len(items); j++ {
				if abs(items[j].ChangePct) > abs(items[i].ChangePct) {
					items[i], items[j] = items[j], items[i]
				}
			}
		}
		if len(items) > 8 {
			items = items[:8]
		}
		snap.Watch = items
	}

	// 今日命中提醒（事件表按触发时间过滤，含已读——复盘看全天）。
	var events []model.AlertEvent
	dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	if err := common.DB.Where("user_id = ? AND triggered_at >= ? AND triggered_at < ?",
		userID, dayStart, dayStart.Add(24*time.Hour)).Limit(20).Find(&events).Error; err == nil {
		for _, e := range events {
			snap.Alerts = append(snap.Alerts, fmt.Sprintf("%s(%s) %s", e.Name, e.Symbol, e.Message))
		}
	}
	if snap.Market == nil && len(snap.Positions) == 0 && len(snap.Watch) == 0 {
		snap.Note = "数据源暂不可用或无持仓自选，仅按已有信息复盘"
	}
	return snap
}

// pickDailyStrategy 按当日涨跌家数为明日推荐选短线策略：
// 强势（涨:跌 ≥ 1.3）追动量、弱势（≤ 0.75）等强势股回踩低吸、中性做热点活跃。
// 无涨跌家数数据时回退动量（与旧行为一致）。
func pickDailyStrategy(snap *reportSnapshot) string {
	if snap == nil || snap.Market == nil || snap.Market.Breadth == nil {
		return "momentum"
	}
	adv, _ := snap.Market.Breadth["advances"].(int)
	dec, _ := snap.Market.Breadth["declines"].(int)
	if dec <= 0 || adv <= 0 {
		return "momentum"
	}
	ratio := float64(adv) / float64(dec)
	switch {
	case ratio >= 1.3:
		return "momentum"
	case ratio <= 0.75:
		return "pullback"
	default:
		return "active"
	}
}

// abs 浮点绝对值（避免为一处调用引入 math 依赖歧义——本包多处自带小工具）。
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// isAdminUser 后台任务里判定 allowPrivate（管理员可用内网自建模型）。
func isAdminUser(userID int64) bool {
	var u model.User
	if err := common.DB.Select("role").First(&u, userID).Error; err != nil {
		return false
	}
	return u.Role == model.RoleAdmin
}

// userPref 读取偏好（失败给默认值，不阻断日报）。
func (s *DailyReportService) userPref(userID int64) model.UserPreference {
	var p model.UserPreference
	if err := common.DB.Where("user_id = ?", userID).First(&p).Error; err != nil {
		return model.UserPreference{DefaultRecCount: 3}
	}
	if p.DefaultRecCount < 3 || p.DefaultRecCount > 5 {
		p.DefaultRecCount = 3
	}
	return p
}

// ---- LLM 复盘 ----

const dailyReviewSystem = `你是个人股票研究助手，为用户生成当日收盘复盘。规则：
1. 只依据用户消息中提供的数据（市场概览/持仓/自选异动/今日提醒），不得编造任何未提供的数据（无财务、无新闻）；数据缺失就如实说明。禁止使用你记忆中的公司/板块信息。
2. 关键判断必须引用数据中的具体数字佐证（如「上涨 3120 家 vs 下跌 1890 家」「主力净流入 23.5 亿」「某持仓今日 -4.2% 已接近计划止损」），让用户可核对。系统会程序化核对你引用的数字，与数据不符的会被标记展示给用户，故不得编造或凭印象填数。
3. 表述为研究参考，不构成投资建议；语气客观，指出风险。
4. 输出严格 JSON（不要 markdown 代码块），schema：
{"summary":"3~5 句当日总结","market_review":"大盘复盘（涨跌家数/资金流向解读）","position_review":"持仓点评（无持仓则一句说明）","watch_review":"自选异动点评（无自选则一句说明）","risk_warnings":["风险提示，1~3 条"],"tomorrow_plan":"明日盘前关注计划（2~4 句）"}`

const dailyReviewRepairHint = `你上一条输出不是合法 JSON 或缺少必填字段。请只输出符合 schema 的 JSON 对象，不要任何解释或代码块包裹。`

// dailyReviewEvidence 核验复盘各段文本引用的数字与复盘快照值域的吻合度（纯计算，可测）。
// 复盘快照无 K 线明细，全量收集即可。提醒文案是 []string（含触发价/MA 值等，由 alert
// 模块拼装）——它们确实作为快照喂给了模型但不是 JSON 数值叶子，必须并入值域，
// 否则模型忠实引用提醒里的价格会被误报为幻觉。
func dailyReviewEvidence(rv *dailyReview, snap *reportSnapshot) *evidenceCheck {
	texts := []string{rv.Summary, rv.MarketReview, rv.PositionReview, rv.WatchReview, rv.TomorrowPlan}
	texts = append(texts, rv.RiskWarnings...)
	vals := snapshotValueSet(snap)
	vals = append(vals, decimalNumbersIn(snap.Alerts)...)
	return verifyEvidenceValues(texts, vals)
}

// callReview 调用 LLM 生成复盘，解析失败 repair 一次。返回（复盘, 总 token, 错误）。
func (s *DailyReportService) callReview(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, snapshotJSON string) (*dailyReview, int, error) {
	messages := []chatMessage{
		{Role: "system", Content: dailyReviewSystem},
		{Role: "user", Content: fmt.Sprintf("今日收盘数据如下（JSON）：\n%s", truncateRunes(snapshotJSON, contextBudgetChars))},
	}
	total := 0
	parse := func(content string) (*dailyReview, error) {
		var rv dailyReview
		if err := json.Unmarshal([]byte(extractJSONObject(content)), &rv); err != nil {
			return nil, err
		}
		if strings.TrimSpace(rv.Summary) == "" {
			return nil, errors.New("summary 为空")
		}
		return &rv, nil
	}

	res, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
		Messages: messages, JSONMode: true, AllowPrivate: allowPrivate,
	})
	if err != nil {
		return nil, total, err
	}
	total += res.Usage.TotalTokens
	if rv, perr := parse(res.Content); perr == nil {
		return rv, total, nil
	}

	// repair 一次。
	messages = append(messages,
		chatMessage{Role: "assistant", Content: truncateRunes(res.Content, 2000)},
		chatMessage{Role: "user", Content: dailyReviewRepairHint},
	)
	res2, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
		Messages: messages, JSONMode: true, AllowPrivate: allowPrivate,
	})
	if err != nil {
		return nil, total, err
	}
	total += res2.Usage.TotalTokens
	rv, perr := parse(res2.Content)
	if perr != nil {
		return nil, total, fmt.Errorf("复盘输出无法解析：%v", perr)
	}
	return rv, total, nil
}

// ---- 卖点提醒 ----

// createSellAlerts 为推荐条目创建到价卖点提醒（止盈 gte / 止损 lte，once）。
// 单条失败忽略（如触达规则数上限）；note 带日期，供 pauseExpiredSellAlerts 过期清理。
func (s *DailyReportService) createSellAlerts(ctx context.Context, userID int64, date string, rec *RecommendationView) {
	for _, item := range rec.Items {
		if item.Detail == nil {
			continue
		}
		base := fmt.Sprintf("%s %s 推荐", reportAlertNotePrefix, date)
		if tp := item.Detail.TakeProfit; tp > 0 {
			_, _ = s.alert.Create(ctx, userID, AlertInput{
				Symbol: item.Symbol, Market: item.Market, Name: item.Name,
				Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: tp, Once: true,
				Note: base + "止盈卖点",
			})
		}
		if sl := item.Detail.StopLoss; sl > 0 {
			_, _ = s.alert.Create(ctx, userID, AlertInput{
				Symbol: item.Symbol, Market: item.Market, Name: item.Name,
				Kind: model.AlertKindPrice, Op: model.AlertOpLTE, Threshold: sl, Once: true,
				Note: base + "止损卖点",
			})
		}
	}
}

// pauseExpiredSellAlerts 把超过有效窗口仍未命中的日报卖点规则置为 paused（不删，留痕）。
func pauseExpiredSellAlerts() {
	cutoff := time.Now().AddDate(0, 0, -reportAlertExpireDays)
	common.DB.Model(&model.AlertRule{}).
		Where("status = ? AND note LIKE ? AND created_at < ?", model.AlertStatusActive, reportAlertNotePrefix+"%", cutoff).
		Update("status", model.AlertStatusPaused)
}

// ---- 后台任务 ----

var dailyReportRunning atomic.Bool

// StartDailyReportJobs 每 10 分钟检查一次：交易日收盘窗口内，为开启日报偏好且
// 当日尚无报告的用户生成。逐用户串行（免费行情源经不起并发打）。
// 服务链在此自建（与 StartAlertJobs 同先例，job 与 API 各持一份无状态实例）。
func StartDailyReportJobs(mgr *datasource.Manager) {
	market := NewMarketService(mgr)
	watchlist := NewWatchlistService(market)
	svc := NewDailyReportService(
		market, watchlist, NewPositionService(market), NewAlertService(market),
		NewRecommendationService(market, watchlist, NewLLMService()), NewLLMService(), NewNotifyService(),
	)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			svc.runAutoOnce(context.Background())
		}
	}()
}

func (s *DailyReportService) runAutoOnce(ctx context.Context) {
	now := time.Now()
	if !inReportWindow(now) || !isTradingDayToday(now) {
		return
	}
	if !dailyReportRunning.CompareAndSwap(false, true) {
		return
	}
	defer dailyReportRunning.Store(false)

	pauseExpiredSellAlerts()

	// 开启日报偏好且账号可用的用户。
	var userIDs []int64
	if err := common.DB.Model(&model.UserPreference{}).
		Joins("JOIN users ON users.id = user_preferences.user_id AND users.status = ?", model.StatusEnabled).
		Where("user_preferences.enable_daily_report = ?", true).
		Pluck("user_preferences.user_id", &userIDs).Error; err != nil {
		common.SysWarn("日报任务读取用户失败: %v", err)
		return
	}
	date := now.Format("2006-01-02")
	for _, uid := range userIDs {
		var cnt int64
		common.DB.Model(&model.DailyReport{}).Where("user_id = ? AND trade_date = ?", uid, date).Count(&cnt)
		if cnt > 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if _, err := s.GenerateFor(cctx, uid, false); err != nil {
			// 失败落一条 failed 记录，避免每 10 分钟反复重试烧 token。
			common.SysWarn("用户 %d 日报生成失败: %v", uid, err)
			common.DB.Create(&model.DailyReport{
				UserID: uid, TradeDate: date, Market: "cn",
				Status: model.ReportStatusFailed, Error: truncateRunes(err.Error(), 500),
			})
		} else {
			common.SysLog("用户 %d 收盘日报已生成", uid)
		}
		cancel()
	}
}
