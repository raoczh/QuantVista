package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	reportWindowStartMin  = 15*60 + 35 // 15:35，收盘数据已稳定
	reportWindowEndMin    = 20 * 60    // 20:00 后不再补生成
	reportAlertNotePrefix = "收盘日报"
	reportAlertExpireDays = 21 // 卖点提醒规则超过该自然日数自动暂停（覆盖短线最长有效期）

	// 异步任务化（2026-07-14）：手动生成立即返回 processing 报告，后台独立 Context 完成后回写。
	reportJobTimeout      = 8 * time.Minute  // 后台任务总 deadline（与原自动日报单用户 deadline 一致）
	reportProcessingStale = 15 * time.Minute // processing 报告超过该时长视为死任务（进程重启遗留），允许重新生成接管
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

	// 测试注入点（nil=默认实现，与 EastMoneyAdapter.fetch 同款先例）：快照聚合与
	// 推荐生成都依赖真实行情上游，端到端单测注入假实现以覆盖并行/回滚/幂等路径。
	snapshotFn func(ctx context.Context, userID int64, date string) *reportSnapshot
	recFn      func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error)
	// nowFn 时钟注入点（nil=time.Now）：15:35 收盘门槛等时间判断经它取当前时刻，
	// 测试可固定时钟——否则「上午跑测试必失败」，测试结果取决于执行时刻。
	nowFn func() time.Time
}

// now 当前时刻（可注入）。所有影响生成决策的时间判断统一走这里。
func (s *DailyReportService) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
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
	EventsReview   string   `json:"events_review"` // N2 今日重要事件解读（事件由硬规则筛出，LLM 只写摘要）
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

type dailyTradingDayState string

const (
	dailyTradingDayOpen    dailyTradingDayState = "open"
	dailyTradingDayClosed  dailyTradingDayState = "closed"
	dailyTradingDayUnknown dailyTradingDayState = "unknown"
)

// dailyTradingDayStatus 日报专用交易日历判定。日报承诺当日收盘口径，缺行或查询失败
// 不能用周一至周五近似，否则节假日或日历同步故障会生成一份伪“今日日报”。
func dailyTradingDayStatus(now time.Time) dailyTradingDayState {
	if common.DB == nil {
		return dailyTradingDayUnknown
	}
	var cal model.TradingCalendar
	res := common.DB.Where("market = ? AND trade_date = ?", "cn", now.Format("2006-01-02")).Limit(1).Find(&cal)
	if res.Error != nil || res.RowsAffected != 1 {
		return dailyTradingDayUnknown
	}
	if cal.IsOpen {
		return dailyTradingDayOpen
	}
	return dailyTradingDayClosed
}

// List 用户的日报列表（排除大字段）。
func (s *DailyReportService) List(userID int64, limit int) ([]model.DailyReport, error) {
	if limit <= 0 || limit > 60 {
		limit = 20
	}
	var rows []model.DailyReport
	err := common.DB.Select("id", "user_id", "trade_date", "market", "status", "prompt_version",
		"llm_config_id", "provider", "model",
		"recommendation_batch_id", "error", "total_tokens", "latency_ms", "created_at", "updated_at").
		Where("user_id = ?", userID).Order("trade_date DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Delete 删除一份日报（仅本人）。生成中的新鲜任务拒删——后台 goroutine 完成后的
// 回写会让删除名存实亡（Save 按主键 UPDATE 落空即静默丢失，行为不可预期）。
// 关联的推荐批次与卖点提醒不级联删（推荐历史独立可见，提醒有自身过期清理）。
func (s *DailyReportService) Delete(userID, id int64) error {
	var r model.DailyReport
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&r).Error; err != nil {
		return errors.New("日报不存在")
	}
	if r.Status == model.ReportStatusProcessing && time.Since(r.UpdatedAt) < reportProcessingStale {
		return refusalErr(RefusalReportProcessing, "日报正在生成中，请等任务结束后再删除")
	}
	return common.DB.Delete(&r).Error
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

// GenerateFor 为用户生成当日日报。
// manual=true（手动生成/重生成）：异步任务化（2026-07-14）——同步段只做休市/LLM/配额
// 校验并落 processing 状态立即返回，复盘+推荐在后台独立 Context 并行执行完成后回写；
// 重生成不再「先删旧报告再生成」，旧报告内容原地保留，双败时状态回滚（旧报告不丢）。
// manual=false（自动 job）：已存在即跳过；调用方已在后台 goroutine 内，保持同步执行。
func (s *DailyReportService) GenerateFor(ctx context.Context, userID int64, manual bool) (*DailyReportView, error) {
	now := s.now()
	switch dailyTradingDayStatus(now) {
	case dailyTradingDayClosed:
		return nil, refusalErr(RefusalMarketClosed, "今日休市，无日报可生成")
	case dailyTradingDayUnknown:
		return nil, refusalErr(RefusalMarketCalendarUnknown, "交易日历暂不可用，无法确认今日是否开市，日报未生成")
	}
	// 收盘数据就绪门槛：正式日报承诺「当日收盘口径」（复盘/涨跌家数/资金流/明日推荐
	// 均按收盘数据组织）。15:35（收盘增量落定，同自动窗口起点）之前手动生成会拿盘中
	// 半截数据占用当天唯一记录，且 15:35 自动任务发现已存在便不再重建——直接拒绝。
	if manual && now.Hour()*60+now.Minute() < reportWindowStartMin {
		return nil, refusalErr(RefusalReportWindow, "收盘日报需在 15:35 收盘数据就绪后生成（当前为盘前/盘中时段，早盘生成会以盘中数据冒充收盘口径）")
	}
	date := now.Format("2006-01-02")

	var existing model.DailyReport
	exists := common.DB.Where("user_id = ? AND trade_date = ?", userID, date).First(&existing).Error == nil
	if exists && !manual {
		return s.assembleView(&existing), nil
	}
	// 幂等防重：生成中的报告直接返回（重复点击不重复建任务烧 token）；超过
	// reportProcessingStale 未回写的 processing 是死任务（进程重启遗留），放行接管重跑。
	if exists && existing.Status == model.ReportStatusProcessing &&
		time.Since(existing.UpdatedAt) < reportProcessingStale {
		return s.assembleView(&existing), nil
	}

	// LLM 配置解析 + 配额熔断（确定性错误立即返回，不建任务；自动生成不占次数，
	// 但额度用尽也不再代烧 token）。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, 0)
	if err != nil {
		return nil, fmt.Errorf("未配置可用的 LLM：%w", err)
	}
	if err := checkQuota(userID); err != nil {
		return nil, err
	}
	plan := reportGenPlan{
		userID: userID, date: date, manual: manual,
		cfg: cfg, apiKey: apiKey,
		allowPrivate: llmAllowPrivate(isAdminUser(userID), cfg), // 回退到管理员配置时按配置所有者放行内网
	}

	// processing 行：当日首份=新建；重生成=旧行原地置 processing（内容字段全部保留，
	// 新报告成功前旧报告始终可看——失败时状态回滚，替代旧版「先删后生成」的丢失风险）。
	report := &model.DailyReport{UserID: userID, TradeDate: date, Market: "cn", Status: model.ReportStatusProcessing}
	if exists {
		report = &existing
		plan.oldStatus = existing.Status
		if plan.oldStatus == model.ReportStatusProcessing {
			plan.oldStatus = model.ReportStatusFailed // 接管的死任务：失败时没有更好的可回滚状态
		}
		// 乐观接管：两个并发请求可能同时越过上面的「新鲜 processing 直接返回」判断
		// （旧报告为 failed/success，或 processing 已 stale），无条件 Update 会双双建任务
		// 重复烧 token。用 (id, status, updated_at) 条件更新，RowsAffected==0 视为已被
		// 另一请求接管——回读当前行按「已在生成中」返回，不再重复起后台任务。
		prevStatus, prevUpdatedAt := existing.Status, existing.UpdatedAt
		report.Status = model.ReportStatusProcessing
		res := common.DB.Model(&model.DailyReport{}).
			Where("id = ? AND status = ? AND updated_at = ?", report.ID, prevStatus, prevUpdatedAt).
			Update("status", model.ReportStatusProcessing)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			var cur model.DailyReport
			if err := common.DB.Where("id = ?", report.ID).First(&cur).Error; err != nil {
				return nil, err
			}
			return s.assembleView(&cur), nil
		}
	} else if err := common.DB.Create(report).Error; err != nil {
		return nil, err // 并发生成的兜底：user+trade_date 唯一索引拒绝第二行
	}

	if !manual {
		return s.runGeneration(ctx, report, plan)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				common.SysWarn("日报后台任务 panic report=%d: %v", report.ID, r)
				common.DB.Model(&model.DailyReport{}).Where("id = ?", report.ID).
					Updates(map[string]any{"status": model.ReportStatusFailed,
						"error": truncateRunes(fmt.Sprintf("生成任务异常终止: %v", r), 500)})
			}
		}()
		// 独立于 HTTP 请求的 Context：浏览器断开/刷新不取消任务；总 deadline 防永久挂起。
		bg, cancel := context.WithTimeout(context.Background(), reportJobTimeout)
		defer cancel()
		if _, err := s.runGeneration(bg, report, plan); err != nil {
			common.SysWarn("用户 %d 日报后台生成失败: %v", userID, err)
		}
	}()
	return s.assembleView(report), nil
}

// reportGenPlan 同步段产出、后台段消费的生成计划。
type reportGenPlan struct {
	userID       int64
	date         string
	manual       bool
	cfg          *model.LLMConfig
	apiKey       string
	allowPrivate bool
	// oldStatus 重生成前的旧状态（空=当日首份）。复盘+推荐双败时回滚到它——
	// 旧报告内容字段全程未被覆盖，回滚状态即等于保住旧报告。
	oldStatus string
}

// runGeneration 生成主体（同步执行；手动路径由 GenerateFor 放进后台 goroutine）：
// 快照 → 复盘与推荐两路并行 → 状态归纳回写 → 卖点提醒/配额/推送收尾。
func (s *DailyReportService) runGeneration(ctx context.Context, report *model.DailyReport, plan reportGenPlan) (*DailyReportView, error) {
	userID, date := plan.userID, plan.date
	start := time.Now()

	snapFn := s.snapshotFn
	if snapFn == nil {
		snapFn = s.buildSnapshot
	}
	recFn := s.recFn
	if recFn == nil {
		recFn = s.rec.GenerateAuto
	}
	snapshot := snapFn(ctx, userID, date)
	snapJSON, _ := json.Marshal(snapshot)
	pref := s.userPref(userID)

	// 复盘与推荐并行（2026-07-14）：两路 LLM 链路互不依赖、互不阻断（单方失败 partial
	// 的语义早已存在），串行只是白白把总时长翻倍。单用户瞬时并发=2，可接受。
	// P0-2 调用关联：日报复盘链路一个 trace（推荐批次自带 trace，经批次 ID 间接串联）。
	trace := newLLMTraceID()
	var (
		wg           sync.WaitGroup
		review       *dailyReview
		reviewTokens int
		reviewRun    *llmRun
		reviewErr    error
		recView      *RecommendationView
		recErr       error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		review, reviewTokens, reviewRun, reviewErr = s.callReview(ctx, userID, date, plan.cfg, plan.apiKey, plan.allowPrivate, string(snapJSON), trace)
	}()
	go func() {
		defer wg.Done()
		// 明日推荐：复用推荐域（短线：买点/止盈/止损/有效期）。策略按当日市场环境
		// 动态选择；筛选条件走用户偏好（Filters=nil 时推荐域自动加载）。
		recReq := RecommendRequest{
			Type: model.RecTypeShortTerm, Market: "cn", Count: pref.DefaultRecCount,
			Strategy: pickDailyStrategy(snapshot),
		}
		recView, recErr = recFn(ctx, userID, plan.allowPrivate, recReq)
	}()
	wg.Wait()

	report.SnapshotJSON = string(snapJSON)
	// M3c：复盘 prompt 版本落库（启用 daily 自定义模板时 -custom 后缀，历史可归因）。
	report.PromptVersion = promptVersionFor(userID, model.PromptModuleDaily, dailyReviewPromptVersion)
	// 生成时使用的 LLM 落库（配置名前端按 llm_config_id 查自己的配置清单解析）。
	report.LLMConfigID = plan.cfg.ID
	report.Provider = plan.cfg.Provider
	report.Model = plan.cfg.Model
	report.TotalTokens = reviewTokens
	// P0-2：trace 与复盘运行元数据落库（双败回滚路径不覆盖旧报告字段，失败尝试的
	// 调用仍可按 trace_id 在 llm_call_logs 查到）。
	report.TraceID = trace
	report.LlmRunJSON = marshalLLMRunManifests(plan.cfg, runEntry(reviewRun, true))
	if reviewErr == nil {
		review.EvidenceCheck = dailyReviewEvidence(review, snapshot) // 信任层回填后随 ReviewJSON 一起落库
		b, _ := json.Marshal(review)
		report.ReviewJSON = string(b)
	}
	if reviewTokens > 0 {
		consumeQuota(userID, reviewTokens, false) // 复盘 token 记审计；次数在末尾按 manual 记一次
	}
	if recErr == nil && recView != nil {
		report.RecommendationBatchID = recView.ID
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

	if plan.manual {
		chargeAction(userID) // 手动生成计 1 次（token 已在各环节以 manual=false 记过审计）
	}

	// 双败 + 重生成：不覆盖旧报告任何内容字段，状态回滚（旧报告依然可看），错误带说明。
	if report.Status == model.ReportStatusFailed && plan.oldStatus != "" {
		msg := truncateRunes("重生成失败（已保留原报告）："+strings.Join(errParts, "；"), 500)
		common.DB.Model(&model.DailyReport{}).Where("id = ?", report.ID).
			Updates(map[string]any{"status": plan.oldStatus, "error": msg})
		return nil, errors.New(strings.Join(errParts, "；"))
	}

	if err := common.DB.Save(report).Error; err != nil {
		return nil, err
	}

	// 卖点提醒：新推荐就绪后先清当日旧规则再建新（重生成场景防重复；推荐失败时
	// 旧规则保留——旧推荐仍在生效期）。
	if recErr == nil && recView != nil {
		common.DB.Where("user_id = ? AND note LIKE ?", userID, reportAlertNotePrefix+" "+date+"%").
			Delete(&model.AlertRule{})
		s.createSellAlerts(ctx, userID, date, recView)
	}

	// 推送摘要（best-effort，受推送通道与偏好总闸控制）。
	if review != nil && pref.EnableNotify && s.notify.HasEnabledChannel(userID) {
		go s.notify.SendMsg(userID, NotifyMessage{
			Title: fmt.Sprintf("收盘日报 %s", date), Content: review.Summary,
			Route: "/daily-report", Kind: NotifyMsgKindReport,
		})
	}
	if report.Status == model.ReportStatusFailed {
		return nil, errors.New(strings.Join(errParts, "；"))
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
	Events    []reportEvent     `json:"events_today,omitempty"` // N2 今日重要事件（硬规则筛出，打分明细随快照可查）
	// F1 明日披露名单：自选∪持仓中次日预约披露财报的标的（文案含名称/代码/报告类型）。
	Disclosures []string `json:"disclosures_tomorrow,omitempty"`
	Note        string   `json:"note,omitempty"`
	// P1 数据水位：核心块（指数/涨跌家数/资金流/情绪温度计）缺失或归属日不符时逐项
	// 登记——随快照落库可复核，prompt 强制声明「今日数据不完整」，旧日期数据不得
	// 冒充「今日完整报告」。
	Deficiencies []string `json:"data_deficiencies,omitempty"`
}

type reportMarket struct {
	Indices  []map[string]any `json:"indices,omitempty"`
	Breadth  map[string]any   `json:"breadth,omitempty"`
	FundFlow map[string]any   `json:"fund_flow,omitempty"`
	// Mood M3a 情绪温度计（涨停池盘后聚合）：连板高度分布/炸板率/昨涨停溢价。
	Mood map[string]any `json:"mood,omitempty"`
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
	// 行情时效（fail-closed）：QuoteStale=true 表示无当前有效行情——当日涨跌/累计盈亏
	// 未知（字段为 0 不代表持平），QuoteNote 说明原因，LLM 不得把该仓计入当前汇总结论。
	QuoteStale bool   `json:"quote_stale,omitempty"`
	QuoteAsOf  string `json:"quote_as_of,omitempty"`
	QuoteNote  string `json:"quote_note,omitempty"`
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
			row := map[string]any{"name": ix.Name, "price": ix.Price, "change_pct": ix.ChangePct,
				"data_time": "", "trade_date": ""}
			if !ix.DataTime.IsZero() {
				row["data_time"] = ix.DataTime.In(time.Local).Format("2006-01-02 15:04:05")
				row["trade_date"] = ix.DataTime.In(time.Local).Format("2006-01-02")
			}
			m.Indices = append(m.Indices, row)
		}
		if ov.Breadth != nil {
			m.Breadth = map[string]any{"advances": ov.Breadth.Advances, "declines": ov.Breadth.Declines,
				"limit_up": ov.Breadth.LimitUp, "limit_down": ov.Breadth.LimitDown,
				"trade_date": ov.Breadth.TradeDate}
			if !ov.Breadth.DataTime.IsZero() {
				m.Breadth["captured_at"] = ov.Breadth.DataTime.In(time.Local).Format("2006-01-02 15:04:05")
			}
		}
		if ov.FundFlow != nil {
			m.FundFlow = map[string]any{"main_net_yi": round2(ov.FundFlow.MainNet / 1e8),
				"trade_date": ov.FundFlow.TradeDate}
			if !ov.FundFlow.DataTime.IsZero() {
				m.FundFlow["captured_at"] = ov.FundFlow.DataTime.In(time.Local).Format("2006-01-02 15:04:05")
			}
		}
		// M3a 情绪温度计：16:35 涨停池 job 先于日报窗口（15:35 起）么？不——日报窗口
		// 15:35~20:00 早段可能取到上一交易日的 mood（trade_date 标注归属日，prompt
		// 已要求按归属日解读）；16:35 采集完成后的日报自然引用当日数据。
		m.Mood = moodBrief()
		snap.Market = m
	}

	// 持仓：List 已走新鲜行情链路（fresh 才有当日涨跌与盈亏；stale 仓透明标注，
	// 不再单独 QuotesFor 富化——旧价的当日涨跌会污染「今日复盘」口径）。
	if views, err := s.position.List(ctx, userID, "holding"); err == nil {
		for _, v := range views {
			p := reportPosition{Symbol: v.Symbol, Name: v.Name, Type: v.PositionType,
				NearStop: v.NearStopLoss, BelowStop: v.BelowStopLoss}
			if v.QuoteOK {
				p.ChangePct = v.DayChangePct
				p.ProfitPct = v.ProfitPct
			} else {
				p.QuoteStale = true
				p.QuoteAsOf = v.QuoteAsOf
				p.QuoteNote = "无当前有效行情（" + orStr(v.StaleReason, "获取失败或已过期") + "）：当日涨跌与累计盈亏未知，不得计入今日汇总或给出操作结论"
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
		snap.Watch = selectReportWatchItems(groups)
	}

	// 今日命中提醒（事件表按触发时间过滤，含已读——复盘看全天）。
	var events []model.AlertEvent
	dayStart, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	dayEnd := dayStart.Add(24 * time.Hour)
	if now := s.now(); now.Before(dayEnd) {
		dayEnd = now
	}
	if err := common.DB.Where("user_id = ? AND triggered_at >= ? AND triggered_at < ?",
		userID, dayStart, dayEnd).Limit(20).Find(&events).Error; err == nil {
		for _, e := range events {
			snap.Alerts = append(snap.Alerts, fmt.Sprintf("%s(%s) %s", e.Name, e.Symbol, e.Message))
		}
	}

	// N2 今日重要事件：4 步硬规则（降噪→三维打分→同主线合并→截断），LLM 只写摘要。
	snap.Events = buildTodayEventsAt(date, s.now())

	// F1 明日披露名单：自选∪持仓中次日预约披露财报的标的（数据来自预约披露表）。
	if d, err := time.ParseInLocation("2006-01-02", date, time.Local); err == nil {
		snap.Disclosures = TomorrowDisclosures(userID, d.AddDate(0, 0, 1).Format("2006-01-02"))
	}

	if snap.Market == nil && len(snap.Positions) == 0 && len(snap.Watch) == 0 {
		snap.Note = "数据源暂不可用或无持仓自选，仅按已有信息复盘"
	}
	snap.Deficiencies = reportDataDeficiencies(snap, date)
	return snap
}

func selectReportWatchItems(groups []WatchlistGroupView) []reportWatchItem {
	var items []reportWatchItem
	for _, group := range groups {
		for _, item := range group.Items {
			if !item.QuoteOK || item.FreshnessStatus != freshStatusFresh {
				continue
			}
			items = append(items, reportWatchItem{Symbol: item.Symbol, Name: item.Name, ChangePct: item.ChangePct})
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
	return items
}

// reportDataDeficiencies P1 数据水位检查（纯函数可测）：核心块逐项对账——缺失/归属日
// 不符（如 15:35~16:35 窗口 mood 仍是上一交易日口径）都显式登记，禁止旧数据静默冒充
// 「今日完整报告」。日期未知/过期的块只保留来源与时点元数据，数值从 LLM 输入和
// evidence 值域中剥离，避免模型无视提示后仍把旧数字写成“今日”并通过数值核验。
func reportDataDeficiencies(snap *reportSnapshot, date string) []string {
	var defs []string
	if snap.Market == nil {
		return append(defs, "市场概览缺失（指数/涨跌家数/资金流均不可用），今日复盘缺市场维度")
	}
	if len(snap.Market.Indices) == 0 {
		defs = append(defs, "指数行情缺失")
	} else {
		indexIssueLogged := false
		for i, ix := range snap.Market.Indices {
			td, hasDate := ix["trade_date"].(string)
			td = strings.TrimSpace(td)
			if !hasDate || strings.TrimSpace(td) == "" {
				note := "指数行情业务日期缺失，无法确认是否为当日收盘口径"
				if !indexIssueLogged {
					defs = append(defs, note)
					indexIssueLogged = true
				}
				snap.Market.Indices[i] = reportStaleMetadata(ix, note)
			} else if td != date {
				note := fmt.Sprintf("指数行情仍为 %s 口径，不代表当日收盘", td)
				if !indexIssueLogged {
					defs = append(defs, note)
					indexIssueLogged = true
				}
				snap.Market.Indices[i] = reportStaleMetadata(ix, note)
			} else if note := reportIndexTimeDeficiency(ix, date); note != "" {
				if !indexIssueLogged {
					defs = append(defs, note)
					indexIssueLogged = true
				}
				snap.Market.Indices[i] = reportStaleMetadata(ix, note)
			}
		}
	}
	if snap.Market.Breadth == nil {
		defs = append(defs, "涨跌家数缺失（赚钱效应维度不可判）")
	} else if td, hasDate := snap.Market.Breadth["trade_date"].(string); !hasDate || strings.TrimSpace(td) == "" {
		note := "涨跌家数业务日期缺失，赚钱效应不可按当日口径判断"
		defs = append(defs, note)
		snap.Market.Breadth = reportStaleMetadata(snap.Market.Breadth, note)
	} else if td, _ := snap.Market.Breadth["trade_date"].(string); td != date {
		note := fmt.Sprintf("涨跌家数仍为 %s 口径，不得参与当日策略选择", td)
		defs = append(defs, note)
		snap.Market.Breadth = reportStaleMetadata(snap.Market.Breadth, note)
	}
	if snap.Market.FundFlow == nil {
		defs = append(defs, "两市资金流缺失")
	} else if td, hasDate := snap.Market.FundFlow["trade_date"].(string); !hasDate || strings.TrimSpace(td) == "" {
		note := "两市资金流业务日期缺失，无法确认是否为当日口径"
		defs = append(defs, note)
		snap.Market.FundFlow = reportStaleMetadata(snap.Market.FundFlow, note)
	} else if td, _ := snap.Market.FundFlow["trade_date"].(string); td != date {
		note := fmt.Sprintf("两市资金流仍为 %s 口径，不代表当日资金流", td)
		defs = append(defs, note)
		snap.Market.FundFlow = reportStaleMetadata(snap.Market.FundFlow, note)
	}
	if snap.Market.Mood == nil {
		defs = append(defs, "情绪温度计（涨停池）今日未就绪")
	} else if td, hasDate := snap.Market.Mood["trade_date"].(string); !hasDate || strings.TrimSpace(td) == "" {
		note := "情绪温度计业务日期缺失，无法确认是否为当日口径"
		defs = append(defs, note)
		snap.Market.Mood = reportStaleMetadata(snap.Market.Mood, note)
	} else if td != date {
		note := fmt.Sprintf("情绪温度计仍为 %s 口径（当日数据 16:35 采集后才有），不代表今日情绪", td)
		defs = append(defs, note)
		snap.Market.Mood = reportStaleMetadata(snap.Market.Mood, note)
	}
	return defs
}

func reportStaleMetadata(block map[string]any, note string) map[string]any {
	out := map[string]any{"stale_for_today": true, "stale_note": note}
	for _, key := range []string{"name", "code", "source", "trade_date", "data_time", "captured_at"} {
		if value, ok := block[key]; ok {
			out[key] = value
		}
	}
	return out
}

func reportIndexTimeDeficiency(index map[string]any, date string) string {
	value, ok := index["data_time"].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "指数行情源时间缺失，无法确认是否为当日收盘口径"
	}
	dataTime, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(value), time.Local)
	if err != nil || dataTime.Format("2006-01-02") != date {
		return "指数行情源时间无法核验，不得作为当日收盘口径"
	}
	if quoteFreshness(dataTime, dataTime, marketStatePostClose, date).Status != freshStatusFresh {
		return fmt.Sprintf("指数行情虽为报告日但仅更新至 %s，未达到收盘口径", dataTime.Format("15:04:05"))
	}
	return ""
}

// pickDailyStrategy 按当日涨跌家数为明日推荐选短线策略：
// 强势（涨:跌 ≥ 1.3）追动量、弱势（≤ 0.75）等强势股回踩低吸、中性做热点活跃。
// 无涨跌家数数据时回退动量（与旧行为一致）。
func pickDailyStrategy(snap *reportSnapshot) string {
	if snap == nil || snap.Market == nil || snap.Market.Breadth == nil {
		return "momentum"
	}
	if td, hasDate := snap.Market.Breadth["trade_date"].(string); !hasDate || strings.TrimSpace(td) == "" || td != snap.TradeDate {
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

// orStr 首个非空字符串（快照标注的缺省文案兜底）。
func orStr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
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

// dailyReviewTaskSeg 复盘角色任务段（L3）：module=daily 自定义模板替换的部分。
// P0-6 起自定义不再整段替换，规则/schema 契约段恒由系统追加。
const dailyReviewTaskSeg = `你是个人股票研究助手，为用户生成当日收盘复盘。`

// dailyReviewContract 复盘模块契约段（L1，不可被自定义模板覆盖）：数据边界/数字核验/
// 各数据块消费纪律/数据水位/输出 schema 与输出纪律。
const dailyReviewContract = `规则：
1. 只依据用户消息中提供的数据（市场概览/持仓/自选异动/今日提醒/今日重要事件），不得编造任何未提供的数据（无财务）；数据缺失就如实说明。禁止使用你记忆中的公司/板块信息。
2. 关键判断必须引用数据中的具体数字佐证（如「上涨 3120 家 vs 下跌 1890 家」「主力净流入 23.5 亿」「某持仓今日 -4.2% 已接近计划止损」），让用户可核对。系统会程序化核对你引用的数字，与数据不符的会被标记展示给用户，故不得编造或凭印象填数。
3. market.mood（若有）是涨停池盘后聚合的情绪温度计：limit_up_count 涨停家数、broken_count/broken_rate 炸板家数与炸板率、streak_dist 连板高度分布（键=连板数）、max_streak 最高连板、yzt_avg_chg/yzt_up_ratio 昨日涨停股今日平均涨跌幅与红盘比例（打板情绪溢价）。market_review 中必须包含一段连板/炸板情绪解读（情绪周期位置、分歧程度、打板赚钱效应），引用具体数字；mood.trade_date 早于今日时注明是上一交易日口径；无 mood 块则跳过这一段，不得臆测。
4. events_today 是程序按来源级别/影响范围/资金敏感度硬规则筛出的今日重要事件（含情绪标签，major=true 为重磅）。events_review 只做解读串联：点出最重要的 2~4 条主线及其对明日盘面的可能影响，引用事件只能复述给出的标题要点，不得展开臆测细节；无事件则一句说明「今日无入选重要事件」。
5. disclosures_tomorrow 是用户自选/持仓中明日预约披露财报的标的名单（程序从预约披露数据筛出）。若非空，tomorrow_plan 中必须提示这些标的明日披露财报、注意业绩波动风险；只能复述名单给出的标的与报告类型，不得预测业绩内容。
6. 表述为研究参考，不构成投资建议；语气客观，指出风险。
7. 数据水位（data_deficiencies，若非空为最高优先级纪律）：这是程序对账出的今日数据缺口清单（指数/涨跌家数/资金流缺失、情绪温度计未就绪或仍为上一交易日口径等）。summary 必须先声明「今日数据不完整」并点出缺失维度；对应段落按数据缺失处理（一句说明缺口，不得引用上一交易日数据冒充今日，也不得臆测补齐）；mood 带 stale_for_today=true 时引用必须注明归属日，不得写成今日情绪。
8. 输出严格 JSON（不要 markdown 代码块），schema：
{"summary":"3~5 句当日总结","market_review":"大盘复盘（涨跌家数/资金流向解读）","events_review":"今日重要事件解读（2~4 句）","position_review":"持仓点评（无持仓则一句说明）","watch_review":"自选异动点评（无自选则一句说明）","risk_warnings":["风险提示，1~3 条"],"tomorrow_plan":"明日盘前关注计划（2~4 句）"}
9. 输出纪律：生成有严格时限，超量输出会被上游截断导致整体作废。各段严格遵守 schema 标注的句数上限，risk_warnings 每条 ≤30 字，精炼比全面重要。`

// dailyReviewSystem 默认复盘系统提示 = 任务段 + 契约段（编译期拼接，与 P0-6 拆分前
// 逐字节一致，默认路径零行为变化；「。规则：」直连无分隔符）。
const dailyReviewSystem = dailyReviewTaskSeg + dailyReviewContract

const dailyReviewRepairHint = `你上一条输出不是合法 JSON 或缺少必填字段。请只输出符合 schema 的 JSON 对象，不要任何解释或代码块包裹。`

// dailyReviewEvidence 核验复盘各段文本引用的数字与复盘快照值域的吻合度（纯计算，可测）。
// 复盘快照无 K 线明细，全量收集即可。提醒文案是 []string（含触发价/MA 值等，由 alert
// 模块拼装）——它们确实作为快照喂给了模型但不是 JSON 数值叶子，必须并入值域，
// 否则模型忠实引用提醒里的价格会被误报为幻觉。
func dailyReviewEvidence(rv *dailyReview, snap *reportSnapshot) *evidenceCheck {
	sections := []evidenceSection{
		{Module: "总结", Text: rv.Summary},
		{Module: "大盘复盘", Text: rv.MarketReview},
		{Module: "事件复盘", Text: rv.EventsReview},
		{Module: "持仓复盘", Text: rv.PositionReview},
		{Module: "自选复盘", Text: rv.WatchReview},
		{Module: "明日计划", Text: rv.TomorrowPlan},
	}
	for _, w := range rv.RiskWarnings {
		sections = append(sections, evidenceSection{Module: "风险提示", Text: w})
	}
	// 快照统一按报告交易日作为 as_of（全字段通配 "" 前缀）。
	vals := snapshotLabeledValues(snap, &snapshotHints{asOf: map[string]string{"": snap.TradeDate}})
	vals = append(vals, textLabeledValues("提醒文案", "context", snap.Alerts)...)
	// N2 事件标题同为文本型合法来源（标题里的小数被复述不算幻觉）。
	titles := make([]string, 0, len(snap.Events))
	for _, e := range snap.Events {
		titles = append(titles, e.Title)
	}
	vals = append(vals, textLabeledValues("事件标题", "context", titles)...)
	check := verifyEvidenceLabeled(sections, vals)
	// ev4（P0-3）：关键结论段（总结）快照佐证计数（复盘快照无 builder 级 unknowns，
	// 数据缺口由 data_deficiencies 水位纪律承担，见 reportDataDeficiencies）。
	markKeySection(check, "总结")
	return check
}

// dailyReviewPromptVersion 复盘系统提示版本（M3c 起随报告落库；改 dailyReviewSystem 时递增）。
// d3: P1 数据水位纪律条（data_deficiencies 强制声明、mood stale_for_today 禁冒充今日）；
// d2: 输出纪律条（段落句数/risk_warnings 字数上限，压单次生成时长进上游 60s 窗口）；d1: 初版。
const dailyReviewPromptVersion = "d3"

// dailyReviewSystemFor 本次复盘的系统提示：P0-6 起 module=daily 的自定义模板是 L3
// 任务段（替换默认角色行），规则/schema 契约段恒由系统追加不可覆盖（占位符 {{date}}
// 宽容渲染）。独立成函数供单测直接断言组装形态。
func dailyReviewSystemFor(userID int64, date string) string {
	if custom, ok := promptOverrideFor(userID, model.PromptModuleDaily, map[string]string{"date": date}); ok {
		return composeCustomTaskPrompt(custom, dailyReviewContract)
	}
	return dailyReviewSystem
}

// callReview 调用 LLM 生成复盘，解析失败 repair 一次。返回（复盘, 总 token, run 元数据, 错误）。
// 首轮与 repair 轮共享同一 run_id（attempt 1/2），P0-2 关联元数据随审计与日报落库。
func (s *DailyReportService) callReview(ctx context.Context, userID int64, date string, cfg *model.LLMConfig, apiKey string, allowPrivate bool, snapshotJSON, traceID string) (*dailyReview, int, *llmRun, error) {
	sys := dailyReviewSystemFor(userID, date)
	messages := []chatMessage{
		{Role: "system", Content: sys},
		{Role: "user", Content: fmt.Sprintf("今日收盘数据如下（JSON）：\n%s", truncateRunes(snapshotJSON, contextBudgetChars))},
	}
	run := newLLMRun(traceID, "", "daily_report", "daily_report.v1",
		promptVersionFor(userID, model.PromptModuleDaily, dailyReviewPromptVersion))
	run.hashData(snapshotJSON)
	run.hashPrompt(messages)
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
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("daily_report", cfg.MaxTokens),
		Messages: messages, JSONMode: true, AllowPrivate: allowPrivate,
		Meta: run.chatMeta(userID, cfg, 1),
	})
	run.record(res, err)
	if err != nil {
		return nil, total, run, err
	}
	total += res.Usage.TotalTokens
	if rv, perr := parse(res.Content); perr == nil {
		return rv, total, run, nil
	}

	// repair 一次（与预算表 daily_report.RepairAttempts=1 声明一致——本函数为固定
	// 「首轮+1 次 repair」结构，改次数须连同预算表同步）。坏输出按模块上限截断回灌。
	messages = append(messages,
		chatMessage{Role: "assistant", Content: moduleRepairFeed("daily_report", res.Content)},
		chatMessage{Role: "user", Content: dailyReviewRepairHint},
	)
	res2, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("daily_report", cfg.MaxTokens),
		Messages: messages, JSONMode: true, AllowPrivate: allowPrivate,
		Repair: true, // repair 轮：契约开启时温度固定 0
		Meta:   run.chatMeta(userID, cfg, 2),
	})
	run.record(res2, err)
	if err != nil {
		return nil, total, run, err
	}
	total += res2.Usage.TotalTokens
	rv, perr := parse(res2.Content)
	if perr != nil {
		run.DegradedReason = "llm_output_invalid"
		return nil, total, run, refusalErrf(RefusalLLMOutputInvalid, "复盘输出无法解析：%v", perr)
	}
	return rv, total, run, nil
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
	now := s.now()
	if !inReportWindow(now) {
		return
	}
	switch dailyTradingDayStatus(now) {
	case dailyTradingDayClosed:
		return
	case dailyTradingDayUnknown:
		common.SysWarn("日报任务跳过：交易日历缺少 %s，无法确认是否开市", now.Format("2006-01-02"))
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
			common.SysWarn("用户 %d 日报生成失败: %v", uid, err)
			// 失败落一条 failed 记录，避免每 10 分钟反复重试烧 token。生成流程可能已
			// 自建行（processing→failed 回写），已有行时跳过（user+trade_date 唯一）。
			var fcnt int64
			common.DB.Model(&model.DailyReport{}).Where("user_id = ? AND trade_date = ?", uid, date).Count(&fcnt)
			if fcnt == 0 {
				common.DB.Create(&model.DailyReport{
					UserID: uid, TradeDate: date, Market: "cn",
					Status: model.ReportStatusFailed, Error: truncateRunes(err.Error(), 500),
				})
			}
		} else {
			common.SysLog("用户 %d 收盘日报已生成", uid)
		}
		cancel()
	}
}
