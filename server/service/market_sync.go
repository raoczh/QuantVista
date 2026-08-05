package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 阶段 2 市场数据补全：日线批量同步、交易日历回填、市场情绪快照，以及驱动它们的后台任务。

const (
	// 批量拉取时对每只股票之间的节流间隔，避免短时间打爆免费数据源。
	syncThrottle = 300 * time.Millisecond
	// 单次批量同步的安全上限，防止 DB 里意外堆积过多标的时任务失控。
	syncMaxStocks = 800
	// 交易日历回填的回看条数（≈4 年交易日）。
	calendarLookback = 1000

	maintenanceDefaultOpenDays = 45
	maintenanceMaxOpenDays     = 60
	maintenanceMaxNaturalDays  = 92
	maintenanceMaxLookbackDays = 400
	maintenanceMaxUniverseRows = 7000
)

// ErrSyncInProgress 已有一轮日线批量同步在跑，拒绝并发重入（免打爆数据源）。
var ErrSyncInProgress = errors.New("已有批量同步任务在进行中")

// ErrMaintenancePlanExpired 表示 dry-run 后本地覆盖事实发生变化，旧计划不得继续执行。
var ErrMaintenancePlanExpired = errors.New("补采计划已失效，请重新 dry-run 确认")

const (
	MaintenanceSyncBars         = "sync_daily_bars"
	MaintenanceBackfillCalendar = "backfill_calendar"
	MaintenanceWideSync         = "sync_market_wide"
)

// MaintenanceRequest 是现有补采 POST 的可选 JSON body。完全无 body 仍走旧兼容路径；
// 一旦提供 body 且非 dry_run，必须携带刚刚 dry-run 得到的 plan_hash。
type MaintenanceRequest struct {
	Market   string `json:"market,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	DryRun   bool   `json:"dry_run,omitempty"`
	PlanHash string `json:"plan_hash,omitempty"`
}

// MaintenancePlan 是可展示、可确认的有限补采计划。样本只返回前 12 个标的；完整集合
// 仅参与 hash，不把大正文塞进响应或审计日志。
type MaintenancePlan struct {
	Task              string   `json:"task"`
	Market            string   `json:"market"`
	From              string   `json:"from"`
	To                string   `json:"to"`
	WindowDays        int      `json:"window_days"`
	TargetCount       int      `json:"target_count"`
	ExpectedCount     int      `json:"expected_count"`
	ExistingCount     int      `json:"existing_count"`
	MissingCount      int      `json:"missing_count"`
	SuspendedCount    int      `json:"suspended_count"`
	EstimatedRequests int      `json:"estimated_requests"`
	Capped            bool     `json:"capped"`
	SampleTargets     []string `json:"sample_targets,omitempty"`
	DifferenceSummary string   `json:"difference_summary"`
	PlanHash          string   `json:"plan_hash"`
	GeneratedAt       string   `json:"generated_at"`
}

// SyncAudit 是同步日志的白名单审计元数据。调用方不得把原始 body 或密钥放进摘要。
type SyncAudit struct {
	TriggerSource    string
	UserID           int64
	ParameterSummary string
	RangeSummary     string
	PlanHash         string
}

// AdminSyncAudit 从已解析的白名单字段构造管理员审计摘要。
func AdminSyncAudit(userID int64, req MaintenanceRequest, legacy bool) SyncAudit {
	trigger := "admin"
	if legacy {
		trigger = "admin_legacy"
	}
	params := "dry_run=" + strconv.FormatBool(req.DryRun)
	if req.PlanHash != "" {
		params += ",plan_hash=" + req.PlanHash
	}
	rng := ""
	if req.From != "" || req.To != "" {
		rng = strings.TrimSpace(req.From) + ".." + strings.TrimSpace(req.To)
	}
	return SyncAudit{TriggerSource: trigger, UserID: userID, ParameterSummary: params, RangeSummary: rng, PlanHash: req.PlanHash}
}

func newSyncLog(task, market string, audit SyncAudit) *model.DataSyncLog {
	trigger := strings.TrimSpace(audit.TriggerSource)
	if trigger == "" {
		trigger = "scheduler"
	}
	return &model.DataSyncLog{
		Task: task, Market: market,
		TriggerSource: trigger, UserID: audit.UserID,
		ParameterSummary: truncate(audit.ParameterSummary, 512),
		RangeSummary:     truncate(audit.RangeSummary, 128),
		PlanHash:         truncate(audit.PlanHash, 64),
	}
}

type preparedMaintenancePlan struct {
	view        MaintenancePlan
	dates       []string
	stocks      []model.Stock
	missingKeys map[string]struct{}
}

type maintenanceHashState struct {
	Version    string   `json:"version"`
	Task       string   `json:"task"`
	Market     string   `json:"market"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Dates      []string `json:"dates"`
	Stocks     []string `json:"stocks,omitempty"`
	Existing   []string `json:"existing,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	Suspended  []string `json:"suspended,omitempty"`
	LocalState []string `json:"local_state,omitempty"`
}

func hashMaintenanceState(state maintenanceHashState) string {
	b, _ := json.Marshal(state)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseMaintenanceDate(v string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), time.Local)
	if err != nil {
		return time.Time{}, errors.New("日期必须为 YYYY-MM-DD")
	}
	return t, nil
}

func validateMaintenanceRange(from, to string, maxDays int) (time.Time, time.Time, error) {
	f, err := parseMaintenanceDate(from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from %w", err)
	}
	t, err := parseMaintenanceDate(to)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to %w", err)
	}
	if t.Before(f) {
		return time.Time{}, time.Time{}, errors.New("from 不得晚于 to")
	}
	days := int(t.Sub(f).Hours()/24) + 1
	if days > maxDays {
		return time.Time{}, time.Time{}, fmt.Errorf("范围最多 %d 个自然日", maxDays)
	}
	today, _ := time.ParseInLocation("2006-01-02", time.Now().Format("2006-01-02"), time.Local)
	if t.After(today) {
		return time.Time{}, time.Time{}, errors.New("to 不得晚于今天")
	}
	if f.Before(today.AddDate(0, 0, -maintenanceMaxLookbackDays)) {
		return time.Time{}, time.Time{}, fmt.Errorf("from 最多回看 %d 个自然日", maintenanceMaxLookbackDays)
	}
	return f, t, nil
}

func reverseStrings(v []string) {
	for i, j := 0, len(v)-1; i < j; i, j = i+1, j-1 {
		v[i], v[j] = v[j], v[i]
	}
}

// syncBarsRunning 保证同一时刻只有一轮批量日线同步（后台定时与手动触发共用）。
var syncBarsRunning atomic.Bool

// IsSyncingBars 当前是否已有一轮批量日线同步在跑，供 controller 触发前预检——否则
// 手动重复触发时 ErrSyncInProgress 被后台 goroutine 吞掉、响应恒 started:true，用户以为
// 又起了一轮。存在极小 TOCTOU 窗口（预检通过到 SyncTrackedDailyBars 内部 CAS 之间），
// 真正的并发重入仍由该 CAS 兜底，此处只为把「已在跑」如实反馈给前端。
func IsSyncingBars() bool {
	return syncBarsRunning.Load()
}

// syncCursor 批量同步的轮转游标（进程内）：记录上一轮同步到的 stocks.id。
// 无游标时每轮都取同一前缀，标的数超过 syncMaxStocks（或任务中途超时取消）时
// 尾部标的永远轮不到；游标让各轮从上次断点继续、扫到尾自动回绕。
var syncCursor atomic.Int64

func normalizeMaintenanceMarket(v string) (string, error) {
	market := strings.ToLower(strings.TrimSpace(v))
	if market == "" {
		market = "cn"
	}
	if market != "cn" {
		return "", errors.New("本批补采只支持 cn 市场")
	}
	return market, nil
}

func maintenanceOpenDates(req MaintenanceRequest) (string, []string, string, string, error) {
	if common.DB == nil {
		return "", nil, "", "", errors.New("数据库不可用")
	}
	market, err := normalizeMaintenanceMarket(req.Market)
	if err != nil {
		return "", nil, "", "", err
	}
	if (strings.TrimSpace(req.From) == "") != (strings.TrimSpace(req.To) == "") {
		return "", nil, "", "", errors.New("from 与 to 必须同时提供")
	}
	var dates []string
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if from == "" {
		if err := common.DB.Model(&model.TradingCalendar{}).Where("market = ? AND is_open = ? AND trade_date <= ?", market, true, time.Now().Format("2006-01-02")).
			Order("trade_date DESC").Limit(maintenanceDefaultOpenDays).Pluck("trade_date", &dates).Error; err != nil {
			return "", nil, "", "", err
		}
		reverseStrings(dates)
		if len(dates) == 0 {
			return "", nil, "", "", errors.New("本地交易日历无开市日，先执行交易日历回填")
		}
		from, to = dates[0], dates[len(dates)-1]
		return market, dates, from, to, nil
	}
	if _, _, err := validateMaintenanceRange(from, to, maintenanceMaxNaturalDays); err != nil {
		return "", nil, "", "", err
	}
	if err := common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date >= ? AND trade_date <= ?", market, true, from, to).
		Order("trade_date").Limit(maintenanceMaxOpenDays+1).Pluck("trade_date", &dates).Error; err != nil {
		return "", nil, "", "", err
	}
	if len(dates) > maintenanceMaxOpenDays {
		return "", nil, "", "", fmt.Errorf("范围最多 %d 个交易日", maintenanceMaxOpenDays)
	}
	return market, dates, from, to, nil
}

func (s *MarketService) buildSyncBarsPlan(req MaintenanceRequest) (*preparedMaintenancePlan, error) {
	market, dates, from, to, err := maintenanceOpenDates(req)
	if err != nil {
		return nil, err
	}
	var stocks []model.Stock
	if err := common.DB.Select("id", "symbol", "market").Where("market = ?", market).
		Order("id").Limit(syncMaxStocks + 1).Find(&stocks).Error; err != nil {
		return nil, err
	}
	capped := len(stocks) > syncMaxStocks
	if capped {
		stocks = stocks[:syncMaxStocks]
	}
	symbols := make([]string, 0, len(stocks))
	for _, st := range stocks {
		symbols = append(symbols, st.Symbol)
	}

	type cell struct{ Symbol, TradeDate string }
	maxCells := len(stocks) * len(dates)
	var existingRows []cell
	if maxCells > 0 {
		if err := common.DB.Model(&model.DailyBar{}).Select("symbol", "trade_date").
			Where("market = ? AND trade_date IN ? AND symbol IN ?", market, dates, symbols).
			Order("trade_date, symbol").Limit(maxCells + 1).Find(&existingRows).Error; err != nil {
			return nil, err
		}
		if len(existingRows) > maxCells {
			return nil, errors.New("日线覆盖查询超过硬上限")
		}
	}
	existing := make(map[string]struct{}, len(existingRows))
	for _, r := range existingRows {
		existing[r.Symbol+"@"+r.TradeDate] = struct{}{}
	}
	type suspendedCell struct{ Symbol, TradeDate string }
	var suspendedRows []suspendedCell
	if maxCells > 0 {
		if err := common.DB.Model(&model.StockUniverseDaily{}).Select("symbol", "trade_date").
			Where("market = ? AND suspended = ? AND trade_date IN ? AND symbol IN ?", market, true, dates, symbols).
			Order("trade_date, symbol").Limit(maxCells + 1).Find(&suspendedRows).Error; err != nil {
			return nil, err
		}
		if len(suspendedRows) > maxCells {
			return nil, errors.New("停牌覆盖查询超过硬上限")
		}
	}
	suspended := make(map[string]struct{}, len(suspendedRows))
	for _, r := range suspendedRows {
		suspended[r.Symbol+"@"+r.TradeDate] = struct{}{}
	}

	missing := make(map[string]struct{})
	targetSet := make(map[string]struct{})
	expectedCount, existingCount := 0, 0
	for _, st := range stocks {
		for _, date := range dates {
			key := st.Symbol + "@" + date
			if _, ok := suspended[key]; ok {
				continue
			}
			expectedCount++
			if _, ok := existing[key]; ok {
				existingCount++
				continue
			}
			missing[key] = struct{}{}
			targetSet[st.Symbol] = struct{}{}
		}
	}
	targetStocks := make([]model.Stock, 0, len(targetSet))
	targetSymbols := make([]string, 0, len(targetSet))
	for _, st := range stocks {
		if _, ok := targetSet[st.Symbol]; ok {
			targetStocks = append(targetStocks, st)
			targetSymbols = append(targetSymbols, st.Symbol)
		}
	}
	existingKeys := make([]string, 0, len(existing))
	for key := range existing {
		existingKeys = append(existingKeys, key)
	}
	missingKeys := make([]string, 0, len(missing))
	for key := range missing {
		missingKeys = append(missingKeys, key)
	}
	suspendedKeys := make([]string, 0, len(suspended))
	for key := range suspended {
		suspendedKeys = append(suspendedKeys, key)
	}
	sort.Strings(existingKeys)
	sort.Strings(missingKeys)
	sort.Strings(suspendedKeys)
	state := maintenanceHashState{
		Version: "p0-3a-v1", Task: MaintenanceSyncBars, Market: market, From: from, To: to,
		Dates: dates, Stocks: symbols, Existing: existingKeys, Missing: missingKeys, Suspended: suspendedKeys,
	}
	view := MaintenancePlan{
		Task: MaintenanceSyncBars, Market: market, From: from, To: to, WindowDays: len(dates),
		TargetCount: len(targetStocks), ExpectedCount: expectedCount, ExistingCount: existingCount,
		MissingCount: len(missing), SuspendedCount: len(suspended), EstimatedRequests: len(targetStocks), Capped: capped,
		DifferenceSummary: fmt.Sprintf("%d 个股票交易日应有，已存在 %d，缺口 %d，已排除停牌 %d；预计请求 %d 只标的", expectedCount, existingCount, len(missing), len(suspended), len(targetStocks)),
		GeneratedAt:       time.Now().Format(time.RFC3339),
	}
	if len(targetSymbols) > 12 {
		view.SampleTargets = append([]string(nil), targetSymbols[:12]...)
	} else {
		view.SampleTargets = append([]string(nil), targetSymbols...)
	}
	view.PlanHash = hashMaintenanceState(state)
	return &preparedMaintenancePlan{view: view, dates: dates, stocks: targetStocks, missingKeys: missing}, nil
}

func calendarRange(req MaintenanceRequest) (string, []string, string, string, error) {
	market, err := normalizeMaintenanceMarket(req.Market)
	if err != nil {
		return "", nil, "", "", err
	}
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if (from == "") != (to == "") {
		return "", nil, "", "", errors.New("from 与 to 必须同时提供")
	}
	if from == "" {
		end, _ := time.ParseInLocation("2006-01-02", time.Now().Format("2006-01-02"), time.Local)
		start := end.AddDate(0, 0, -(maintenanceMaxNaturalDays - 1))
		from, to = start.Format("2006-01-02"), end.Format("2006-01-02")
	}
	f, t, err := validateMaintenanceRange(from, to, maintenanceMaxNaturalDays)
	if err != nil {
		return "", nil, "", "", err
	}
	dates := make([]string, 0, maintenanceMaxNaturalDays)
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	return market, dates, from, to, nil
}

func (s *MarketService) buildCalendarPlan(req MaintenanceRequest) (*preparedMaintenancePlan, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	market, dates, from, to, err := calendarRange(req)
	if err != nil {
		return nil, err
	}
	var rows []model.TradingCalendar
	if err := common.DB.Select("trade_date", "is_open").Where("market = ? AND trade_date >= ? AND trade_date <= ?", market, from, to).
		Order("trade_date").Limit(len(dates) + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > len(dates) {
		return nil, errors.New("日历计划查询超过硬上限")
	}
	existing := make(map[string]bool, len(rows))
	localState := make([]string, 0, len(rows))
	for _, row := range rows {
		existing[row.TradeDate] = row.IsOpen
		localState = append(localState, row.TradeDate+":"+strconv.FormatBool(row.IsOpen))
	}
	missing := make([]string, 0)
	for _, date := range dates {
		if _, ok := existing[date]; !ok {
			missing = append(missing, date)
		}
	}
	state := maintenanceHashState{
		Version: "p0-3a-v1", Task: MaintenanceBackfillCalendar, Market: market, From: from, To: to,
		Dates: dates, Missing: missing, LocalState: localState,
	}
	view := MaintenancePlan{
		Task: MaintenanceBackfillCalendar, Market: market, From: from, To: to, WindowDays: len(dates),
		TargetCount: len(missing), ExpectedCount: len(dates), ExistingCount: len(rows), MissingCount: len(missing),
		EstimatedRequests: 1,
		DifferenceSummary: fmt.Sprintf("范围内 %d 个自然日，本地已有 %d，待补/待核对 %d；执行时以上游开市日集合订正，未来未知工作日不写成休市", len(dates), len(rows), len(missing)),
		GeneratedAt:       time.Now().Format(time.RFC3339),
	}
	if len(missing) > 12 {
		view.SampleTargets = append([]string(nil), missing[:12]...)
	} else {
		view.SampleTargets = append([]string(nil), missing...)
	}
	view.PlanHash = hashMaintenanceState(state)
	return &preparedMaintenancePlan{view: view, dates: dates}, nil
}

func (s *MarketService) buildWidePlan(req MaintenanceRequest) (*preparedMaintenancePlan, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	market, err := normalizeMaintenanceMarket(req.Market)
	if err != nil {
		return nil, err
	}
	expected := wideExpectedDate(time.Now())
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if (from == "") != (to == "") {
		return nil, errors.New("from 与 to 必须同时提供")
	}
	if from == "" {
		from, to = expected, expected
	}
	if _, _, err := validateMaintenanceRange(from, to, 1); err != nil {
		return nil, err
	}
	if from != expected || to != expected {
		return nil, fmt.Errorf("全市场快照接口只能补当前应有交易日 %s，历史范围请用日线初始化/批量补采", expected)
	}
	var states []string
	if err := common.DB.Model(&model.MarketSyncState{}).Where("market = ?", market).Order("symbol").Limit(maintenanceMaxUniverseRows+1).Pluck("symbol", &states).Error; err != nil {
		return nil, err
	}
	if len(states) > maintenanceMaxUniverseRows {
		return nil, errors.New("全市场宇宙超过计划查询硬上限")
	}
	var bars []string
	if len(states) > 0 {
		if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND trade_date = ? AND symbol IN ?", market, expected, states).
			Order("symbol").Limit(maintenanceMaxUniverseRows+1).Pluck("symbol", &bars).Error; err != nil {
			return nil, err
		}
	}
	if len(bars) > maintenanceMaxUniverseRows {
		return nil, errors.New("全市场日线超过计划查询硬上限")
	}
	var suspended []string
	if len(states) > 0 {
		if err := common.DB.Model(&model.StockUniverseDaily{}).Where("market = ? AND trade_date = ? AND suspended = ? AND symbol IN ?", market, expected, true, states).
			Order("symbol").Limit(maintenanceMaxUniverseRows+1).Pluck("symbol", &suspended).Error; err != nil {
			return nil, err
		}
	}
	if len(suspended) > maintenanceMaxUniverseRows {
		return nil, errors.New("全市场停牌行超过计划查询硬上限")
	}
	barSet := make(map[string]struct{}, len(bars))
	for _, symbol := range bars {
		barSet[symbol] = struct{}{}
	}
	suspendedSet := make(map[string]struct{}, len(suspended))
	for _, symbol := range suspended {
		suspendedSet[symbol] = struct{}{}
	}
	missing := make([]string, 0)
	for _, symbol := range states {
		if _, ok := suspendedSet[symbol]; ok {
			continue
		}
		if _, ok := barSet[symbol]; !ok {
			missing = append(missing, symbol)
		}
	}
	state := maintenanceHashState{
		Version: "p0-3a-v1", Task: MaintenanceWideSync, Market: market, From: from, To: to,
		Dates: []string{expected}, Stocks: states, Existing: bars, Missing: missing, Suspended: suspended,
	}
	view := MaintenancePlan{
		Task: MaintenanceWideSync, Market: market, From: from, To: to, WindowDays: 1,
		TargetCount: len(states), ExpectedCount: len(states) - len(suspended), ExistingCount: len(bars),
		MissingCount: len(missing), SuspendedCount: len(suspended), EstimatedRequests: 1,
		DifferenceSummary: fmt.Sprintf("%s 宇宙 %d 只，已有日线 %d，已知停牌 %d，待同步/核对 %d", expected, len(states), len(bars), len(suspended), len(missing)),
		GeneratedAt:       time.Now().Format(time.RFC3339),
	}
	if len(missing) > 12 {
		view.SampleTargets = append([]string(nil), missing[:12]...)
	} else {
		view.SampleTargets = append([]string(nil), missing...)
	}
	view.PlanHash = hashMaintenanceState(state)
	return &preparedMaintenancePlan{view: view, dates: []string{expected}}, nil
}

func (s *MarketService) prepareMaintenancePlan(task string, req MaintenanceRequest) (*preparedMaintenancePlan, error) {
	switch task {
	case MaintenanceSyncBars:
		return s.buildSyncBarsPlan(req)
	case MaintenanceBackfillCalendar:
		return s.buildCalendarPlan(req)
	case MaintenanceWideSync:
		return s.buildWidePlan(req)
	default:
		return nil, errors.New("未知补采任务")
	}
}

// PlanMaintenance 只读本地库生成计划，不扫描上游。
func (s *MarketService) PlanMaintenance(task string, req MaintenanceRequest) (*MaintenancePlan, error) {
	prepared, err := s.prepareMaintenancePlan(task, req)
	if err != nil {
		return nil, err
	}
	return &prepared.view, nil
}

// ValidateMaintenancePlan 在真正执行前用本地事实重算 hash。
func (s *MarketService) ValidateMaintenancePlan(task string, req MaintenanceRequest) error {
	if len(req.PlanHash) != 64 {
		return errors.New("执行补采前必须提供 64 位 plan_hash")
	}
	prepared, err := s.prepareMaintenancePlan(task, req)
	if err != nil {
		return err
	}
	if !strings.EqualFold(prepared.view.PlanHash, req.PlanHash) {
		return ErrMaintenancePlanExpired
	}
	return nil
}

// RunMarketWidePlan 在实际请求上游前重算计划；历史范围会在 buildWidePlan 阶段被拒绝，
// 不把“只能补当日快照”伪装成历史补采能力。
func (s *MarketService) RunMarketWidePlan(ctx context.Context, req MaintenanceRequest, audit SyncAudit) (*model.DataSyncLog, error) {
	prepared, err := s.buildWidePlan(req)
	if err != nil {
		return nil, err
	}
	if len(req.PlanHash) != 64 || !strings.EqualFold(prepared.view.PlanHash, req.PlanHash) {
		return nil, ErrMaintenancePlanExpired
	}
	audit.PlanHash = req.PlanHash
	audit.RangeSummary = prepared.view.From + ".." + prepared.view.To
	audit.ParameterSummary = fmt.Sprintf("dry_run=false,universe=%d,missing=%d", prepared.view.TargetCount, prepared.view.MissingCount)
	return s.SyncMarketWideWithAudit(ctx, audit)
}

// RecordMaintenanceFailure 记录管理员确认后的执行拒绝/失败，不记录原始请求正文。
func (s *MarketService) RecordMaintenanceFailure(task, market string, audit SyncAudit, err error) {
	if err == nil {
		return
	}
	log := newSyncLog(task, market, audit)
	log.Status = "failed"
	log.Failed = 1
	log.Message = truncate(err.Error(), 512)
	s.recordSyncLog(log)
}

func applySyncAudit(log *model.DataSyncLog, audit SyncAudit) {
	if log == nil {
		return
	}
	meta := newSyncLog(log.Task, log.Market, audit)
	log.TriggerSource = meta.TriggerSource
	log.UserID = meta.UserID
	log.ParameterSummary = meta.ParameterSummary
	log.RangeSummary = meta.RangeSummary
	log.PlanHash = meta.PlanHash
}

// SyncMarketWideWithAudit 复用既有全市场同步，不改其采集行为；任务落库后只更新五个
// 白名单审计字段。这样 controller 可记录管理员，又不让原始 body 进入日志。
func (s *MarketService) SyncMarketWideWithAudit(ctx context.Context, audit SyncAudit) (*model.DataSyncLog, error) {
	log, err := s.SyncMarketWide(ctx)
	if log == nil {
		return nil, err
	}
	applySyncAudit(log, audit)
	if common.DB != nil && log.ID > 0 {
		if updateErr := common.DB.Model(&model.DataSyncLog{}).Where("id = ?", log.ID).Updates(map[string]any{
			"trigger_source": log.TriggerSource, "user_id": log.UserID,
			"parameter_summary": log.ParameterSummary, "range_summary": log.RangeSummary,
			"plan_hash": log.PlanHash,
		}).Error; updateErr != nil {
			common.SysWarn("补写全市场同步审计失败: %v", updateErr)
		}
	}
	return log, err
}

// StartMarketWideInitWithAudit 保持原异步初始化入口不变，并单独记录管理员接受触发的
// 事实。最终执行日志仍由原任务写入；两者不伪装成统一 JobRun。
func (s *MarketService) StartMarketWideInitWithAudit(audit SyncAudit) error {
	if err := s.StartMarketWideInit(); err != nil {
		return err
	}
	log := newSyncLog("init_market_history_trigger", "cn", audit)
	log.Status = "success"
	log.Total = 1
	log.Succeeded = 1
	log.Message = "已接受全市场历史初始化/续跑请求"
	s.recordSyncLog(log)
	return nil
}

// SyncTrackedDailyBars 批量同步"已跟踪"股票（DB stocks 表内、即用户查过/持有的标的）的日线。
// 这是"全市场日线批量同步"的个人自用版：不主动抓全 5000 只（会长时间打免费源），
// 而是覆盖用户实际关心的标的；同步结果写入 data_sync_logs 供审计。
func (s *MarketService) SyncTrackedDailyBars(ctx context.Context, market string, barLimit int) (*model.DataSyncLog, error) {
	return s.SyncTrackedDailyBarsWithAudit(ctx, market, barLimit, SyncAudit{TriggerSource: "scheduler", ParameterSummary: "bar_limit=" + strconv.Itoa(barLimit)})
}

// SyncTrackedDailyBarsWithAudit 是旧版“最近 N 根”同步路径；用于完全无 body 的兼容调用
// 和定时任务。有限范围的新调用走 RunSyncBarsPlan。
func (s *MarketService) SyncTrackedDailyBarsWithAudit(ctx context.Context, market string, barLimit int, audit SyncAudit) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !syncBarsRunning.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer syncBarsRunning.Store(false)

	if barLimit <= 0 || barLimit > 1000 {
		barLimit = 120
	}
	start := time.Now()

	fetch := func(afterID int64, limit int) ([]model.Stock, error) {
		var rows []model.Stock
		q := common.DB.Model(&model.Stock{}).Where("id > ?", afterID).Order("id")
		if market != "" {
			q = q.Where("market = ?", market)
		}
		err := q.Limit(limit).Find(&rows).Error
		return rows, err
	}
	stocks, err := fetch(syncCursor.Load(), syncMaxStocks)
	if err != nil {
		return nil, err
	}
	// 到尾回绕：从头补齐剩余额度。
	if len(stocks) < syncMaxStocks {
		head, err := fetch(0, syncMaxStocks-len(stocks))
		if err == nil {
			stocks = append(stocks, head...)
		}
	}

	log := newSyncLog(MaintenanceSyncBars, market, audit)
	log.Total = len(stocks)
	var firstErr string
	for _, st := range stocks {
		select {
		case <-ctx.Done():
			log.Message = truncate("任务取消: "+ctx.Err().Error(), 512)
			log.DurationMs = time.Since(start).Milliseconds()
			log.Status = statusOf(log)
			s.recordSyncLog(log)
			return log, ctx.Err()
		default:
		}
		if _, err := s.GetDailyBars(ctx, st.Market, st.Symbol, barLimit); err != nil {
			log.Failed++
			if firstErr == "" {
				firstErr = st.Symbol + ": " + err.Error()
			}
		} else {
			log.Succeeded++
		}
		syncCursor.Store(st.ID) // 中途取消也从断点续跑
		time.Sleep(syncThrottle)
	}

	log.DurationMs = time.Since(start).Milliseconds()
	log.Message = truncate(firstErr, 512)
	log.Status = statusOf(log)
	s.recordSyncLog(log)
	return log, nil
}

func (s *MarketService) syncBarsFetchLimit(market string, dates []string) int {
	if len(dates) == 0 || common.DB == nil {
		return 1
	}
	var n int64
	common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date >= ? AND trade_date <= ?", market, true, dates[0], time.Now().Format("2006-01-02")).
		Limit(1000).Count(&n)
	limit := int(n) + 5
	if limit < 30 {
		limit = 30
	}
	if limit > 1000 {
		limit = 1000
	}
	return limit
}

// RunSyncBarsPlan 在执行瞬间重建本地计划并校验 hash，随后只补计划中的缺口日期。
func (s *MarketService) RunSyncBarsPlan(ctx context.Context, req MaintenanceRequest, audit SyncAudit) (*model.DataSyncLog, error) {
	prepared, err := s.buildSyncBarsPlan(req)
	if err != nil {
		return nil, err
	}
	if len(req.PlanHash) != 64 || !strings.EqualFold(prepared.view.PlanHash, req.PlanHash) {
		return nil, ErrMaintenancePlanExpired
	}
	if !syncBarsRunning.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer syncBarsRunning.Store(false)

	audit.PlanHash = req.PlanHash
	audit.RangeSummary = prepared.view.From + ".." + prepared.view.To
	audit.ParameterSummary = fmt.Sprintf("dry_run=false,target_stocks=%d,missing_cells=%d", prepared.view.TargetCount, prepared.view.MissingCount)
	log := newSyncLog(MaintenanceSyncBars, prepared.view.Market, audit)
	log.Total = len(prepared.stocks)
	start := time.Now()
	if len(prepared.stocks) == 0 {
		log.Status = "success"
		log.Message = "计划内无日线缺口"
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, nil
	}

	fetchLimit := s.syncBarsFetchLimit(prepared.view.Market, prepared.dates)
	var firstErr string
	for _, st := range prepared.stocks {
		if err := ctx.Err(); err != nil {
			log.Message = truncate("任务取消: "+err.Error(), 512)
			log.DurationMs = time.Since(start).Milliseconds()
			log.Status = statusOf(log)
			s.recordSyncLog(log)
			return log, err
		}
		bars, fetchErr := s.mgr.GetDailyBars(ctx, st.Market, st.Symbol, fetchLimit)
		filtered := make([]datasource.Bar, 0, len(prepared.dates))
		if fetchErr == nil {
			for _, bar := range bars {
				if _, ok := prepared.missingKeys[st.Symbol+"@"+bar.TradeDate]; ok {
					filtered = append(filtered, bar)
				}
			}
			if len(filtered) == 0 {
				fetchErr = datasource.ErrNoData
			} else {
				fetchErr = s.persistDailyBars(ctx, st.Market, st.Symbol, filtered)
			}
		}
		if fetchErr != nil {
			log.Failed++
			if firstErr == "" {
				firstErr = st.Symbol + ": " + fetchErr.Error()
			}
		} else {
			log.Succeeded++
		}
		timer := time.NewTimer(syncThrottle)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	log.DurationMs = time.Since(start).Milliseconds()
	log.Status = statusOf(log)
	log.Message = truncate(fmt.Sprintf("计划缺口 %d 个股票交易日；标的成功 %d/%d", prepared.view.MissingCount, log.Succeeded, log.Total)+orStr(func() string {
		if firstErr == "" {
			return ""
		}
		return "；首错 " + firstErr
	}(), ""), 512)
	s.recordSyncLog(log)
	return log, nil
}

// BackfillCalendar 回填交易日历：用上证指数日线得到开市日集合，
// 再把区间内其余日期（周末/节假日）补为休市日（is_open=false），形成完整日历。
// 未来覆盖：指数日线只到最近已收盘交易日，未来的开市/节假日无公开数据源可回填——
// 仅把未来 45 天内的周六/周日预写为休市（A 股周末恒不开市，即使调休工作日），
// 未来工作日不写行（保持「无日历按周一~五近似」的诚实退化，预写开市会把未来
// 节假日误判为交易日、放大 stale 误报）。
func (s *MarketService) BackfillCalendar(ctx context.Context, market string) (*model.DataSyncLog, error) {
	return s.BackfillCalendarWithAudit(ctx, market, SyncAudit{TriggerSource: "scheduler", ParameterSummary: "lookback=" + strconv.Itoa(calendarLookback)})
}

// BackfillCalendarWithAudit 保留旧版全回看行为，供无 body 客户端与启动修复使用。
func (s *MarketService) BackfillCalendarWithAudit(ctx context.Context, market string, audit SyncAudit) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	start := time.Now()
	log := newSyncLog(MaintenanceBackfillCalendar, market, audit)

	days, err := s.mgr.GetTradingDays(ctx, market, calendarLookback)
	if err != nil {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}

	open := make(map[string]struct{}, len(days))
	var minDate, maxDate string
	for _, d := range days {
		open[d] = struct{}{}
		if minDate == "" || d < minDate {
			minDate = d
		}
		if d > maxDate {
			maxDate = d
		}
	}
	from, err1 := time.ParseInLocation("2006-01-02", minDate, time.Local)
	to, err2 := time.ParseInLocation("2006-01-02", maxDate, time.Local)
	if err1 != nil || err2 != nil {
		log.Status = "failed"
		log.Message = "交易日日期解析失败"
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, errors.New(log.Message)
	}

	rows := make([]model.TradingCalendar, 0, 512)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		_, isOpen := open[ds]
		rows = append(rows, model.TradingCalendar{Market: market, TradeDate: ds, IsOpen: isOpen})
	}
	// 未来周末预写休市（maxDate 之后 45 天内）：周末不开市是确定事实，预写后跨周末
	// 的新鲜度判定（isTradingDayToday/prevOpenTradeDate）不必依赖周一~五近似；
	// 工作日节假日无数据源，不预写（见函数头注释）。
	weekendEnd := time.Now().AddDate(0, 0, 45)
	for d := to.AddDate(0, 0, 1); !d.After(weekendEnd); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			rows = append(rows, model.TradingCalendar{Market: market, TradeDate: d.Format("2006-01-02"), IsOpen: false})
		}
	}

	// 显式 Select 强制写入 is_open，即使历史 DB 列上仍残留 default:true 也不会漏写休市日。
	if err := common.DB.
		Select("Market", "TradeDate", "IsOpen").
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
		}).CreateInBatches(rows, 200).Error; err != nil {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}

	log.Total = len(rows)
	log.Succeeded = len(days) // 开市日数
	log.Status = "success"
	futureWeekends := len(rows) - int(to.Sub(from).Hours()/24) - 1
	log.Message = truncate(minDate+" ~ "+maxDate+" 共 "+strconv.Itoa(len(rows)-futureWeekends)+" 天（开市 "+strconv.Itoa(len(days))+"）；另预写未来周末休市 "+strconv.Itoa(futureWeekends)+" 天（工作日节假日无数据源，跨长假仍按周一~五近似）", 512)
	log.DurationMs = time.Since(start).Milliseconds()
	s.recordSyncLog(log)
	return log, nil
}

// RunCalendarPlan 校验 dry-run 计划后，只在有限范围内订正本地日历。上游最近交易日
// 之后的周末可确定为休市；未来工作日保持 unknown，不伪写 is_open=false。
func (s *MarketService) RunCalendarPlan(ctx context.Context, req MaintenanceRequest, audit SyncAudit) (*model.DataSyncLog, error) {
	prepared, err := s.buildCalendarPlan(req)
	if err != nil {
		return nil, err
	}
	if len(req.PlanHash) != 64 || !strings.EqualFold(prepared.view.PlanHash, req.PlanHash) {
		return nil, ErrMaintenancePlanExpired
	}
	audit.PlanHash = req.PlanHash
	audit.RangeSummary = prepared.view.From + ".." + prepared.view.To
	audit.ParameterSummary = fmt.Sprintf("dry_run=false,range_days=%d,missing_rows=%d", prepared.view.WindowDays, prepared.view.MissingCount)
	log := newSyncLog(MaintenanceBackfillCalendar, prepared.view.Market, audit)
	start := time.Now()

	days, err := s.mgr.GetTradingDays(ctx, prepared.view.Market, calendarLookback)
	if err != nil {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}
	open := make(map[string]struct{}, len(days))
	maxSourceDate := ""
	for _, date := range days {
		open[date] = struct{}{}
		if date > maxSourceDate {
			maxSourceDate = date
		}
	}
	rows := make([]model.TradingCalendar, 0, len(prepared.dates))
	unresolved := 0
	for _, date := range prepared.dates {
		if date <= maxSourceDate {
			_, isOpen := open[date]
			rows = append(rows, model.TradingCalendar{Market: prepared.view.Market, TradeDate: date, IsOpen: isOpen})
			continue
		}
		d, _ := time.ParseInLocation("2006-01-02", date, time.Local)
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			rows = append(rows, model.TradingCalendar{Market: prepared.view.Market, TradeDate: date, IsOpen: false})
		} else {
			unresolved++
		}
	}
	if err := common.DB.Select("Market", "TradeDate", "IsOpen").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
	}).CreateInBatches(rows, 200).Error; err != nil && err != gorm.ErrEmptySlice {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}
	log.Total = len(prepared.dates)
	log.Succeeded = len(rows)
	log.Failed = unresolved
	log.Status = statusOf(log)
	log.Message = truncate(fmt.Sprintf("%s ~ %s：订正 %d 天，未来未知工作日保留 %d 天", prepared.view.From, prepared.view.To, len(rows), unresolved), 512)
	log.DurationMs = time.Since(start).Milliseconds()
	s.recordSyncLog(log)
	return log, nil
}

// SnapshotMarket 拉取当前涨跌家数并落库为一条市场情绪快照，形成历史序列。
// 与上一条完全相同（同交易日且各家数未变，典型如收盘后）则跳过，避免非交易时段堆积重复行。
func (s *MarketService) SnapshotMarket(ctx context.Context, market string) (*model.MarketSnapshot, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	b, err := s.mgr.GetBreadth(ctx, market)
	if err != nil {
		return nil, err
	}
	snap := &model.MarketSnapshot{
		Market:    market,
		TradeDate: b.TradeDate,
		Advances:  b.Advances,
		Declines:  b.Declines,
		Unchanged: b.Unchanged,
		LimitUp:   b.LimitUp,
		LimitDown: b.LimitDown,
		Source:    b.Source,
		DataTime:  b.DataTime,
	}
	if last, err := s.LatestSnapshot(market); err == nil && last != nil && sameBreadth(last, snap) {
		return last, nil // 数据未变，复用上一条
	}
	if err := common.DB.Create(snap).Error; err != nil {
		return nil, err
	}
	return snap, nil
}

// SnapshotMarketWithAudit 仅供管理员手动入口：后台 10 分钟快照仍不刷审计表，手动操作
// 则记录触发者与有限摘要。
func (s *MarketService) SnapshotMarketWithAudit(ctx context.Context, market string, audit SyncAudit) (*model.MarketSnapshot, error) {
	start := time.Now()
	snap, err := s.SnapshotMarket(ctx, market)
	log := newSyncLog("snapshot_market", market, audit)
	log.Total = 1
	log.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		log.Status = "failed"
		log.Failed = 1
		log.Message = truncate(err.Error(), 512)
	} else {
		log.Status = "success"
		log.Succeeded = 1
		if snap != nil {
			log.RangeSummary = snap.TradeDate
			log.Message = truncate("市场情绪快照 "+snap.TradeDate, 512)
		}
	}
	s.recordSyncLog(log)
	return snap, err
}

// sameBreadth 判断两条快照的涨跌家数是否一致（用于去重）。
func sameBreadth(a, b *model.MarketSnapshot) bool {
	return a.TradeDate == b.TradeDate &&
		a.Advances == b.Advances && a.Declines == b.Declines && a.Unchanged == b.Unchanged &&
		a.LimitUp == b.LimitUp && a.LimitDown == b.LimitDown
}

// LatestSnapshot 返回某市场最近一条情绪快照（无则 nil）。
func (s *MarketService) LatestSnapshot(market string) (*model.MarketSnapshot, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var snap model.MarketSnapshot
	err := common.DB.Where("market = ?", market).Order("data_time DESC").First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// RecentSyncLogs 返回最近的数据同步任务日志（供管理员排查数据缺口）。
func (s *MarketService) RecentSyncLogs(limit int) ([]model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var logs []model.DataSyncLog
	err := common.DB.Order("created_at DESC").Limit(limit).Find(&logs).Error
	return logs, err
}

func (s *MarketService) recordSyncLog(log *model.DataSyncLog) {
	if common.DB == nil || log == nil {
		return
	}
	if err := common.DB.Create(log).Error; err != nil {
		common.SysWarn("写 data_sync_logs 失败: %v", err)
	}
}

// statusOf 依据成功/失败计数判定同步状态。
func statusOf(log *model.DataSyncLog) string {
	switch {
	case log.Total == 0:
		return "success"
	case log.Failed == 0:
		return "success"
	case log.Succeeded == 0:
		return "failed"
	default:
		return "partial"
	}
}

// StartMarketJobs 启动市场数据后台任务：
//   - 启动时若日历为空则回填一次；
//   - 每 10 分钟落一条市场情绪快照（数据源不可用时静默跳过）；
//   - 每 6 小时批量同步已跟踪股票日线。
//
// 均为个人自用低频任务，失败仅记日志不影响主流程。
func StartMarketJobs(mgr *datasource.Manager) {
	svc := NewMarketService(mgr)
	const market = "cn"

	// 启动时：日历为空才回填（避免每次重启都全量刷）。
	go func() {
		if common.DB == nil {
			return
		}
		var n int64
		common.DB.Model(&model.TradingCalendar{}).Where("market = ?", market).Count(&n)
		if n > 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := svc.BackfillCalendar(ctx, market); err != nil {
			common.SysWarn("启动回填交易日历失败: %v", err)
		} else {
			common.SysLog("启动回填交易日历完成")
		}
	}()

	// 市场情绪快照：每 10 分钟一次。
	go func() {
		snapshot := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := svc.SnapshotMarket(ctx, market); err != nil {
				common.SysDebug("市场情绪快照跳过（数据源不可用）: %v", err)
			}
		}
		snapshot()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			snapshot()
		}
	}()

	// 已跟踪股票日线批量同步：启动 5 分钟后先跑一次，之后每 6 小时一次。
	// 首跑不等 6 小时——频繁部署/重启的进程可能永远等不到第一轮 ticker，
	// 已跟踪标的的日线会长期停更（5 分钟缓冲避开启动期抢资源）。
	// 超时预算：800 只 × (300ms 节流 + 抓取) 最坏可到 20 分钟以上，给足 30 分钟；
	// 即便中途取消，游标也保证下一轮从断点续跑。
	go func() {
		runSync := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if log, err := svc.SyncTrackedDailyBars(ctx, market, 120); err != nil {
				common.SysWarn("批量同步日线失败: %v", err)
			} else if log.Total > 0 {
				common.SysLog("批量同步日线完成: 共 %d 成功 %d 失败 %d", log.Total, log.Succeeded, log.Failed)
			}
		}
		time.Sleep(5 * time.Minute)
		runSync()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			runSync()
		}
	}()

	// M1 全市场日线：每日 16:10（交易日）clist 增量落当日 bar + 除权初筛；
	// 增量后若宇宙内仍有 pending（首轮部署/新股/重锚失败回退），自动推进历史初始化
	//（异步、防重入、断点续传）。16:10 避开收盘竞价尾流，且与 19:05 finance job 错峰。
	// 启动补跑：进程在 16:10 后启动（当日部署/重启）会睡到次日 16:10、当天增量被
	// 静默错过——启动时若「交易日且已过 16:10 且今日无成功增量记录」先补跑一次。
	go func() {
		if common.DB == nil {
			return
		}
		runWide := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if log, err := svc.SyncMarketWide(ctx); err != nil && !errors.Is(err, ErrSyncInProgress) {
				common.SysWarn("全市场日线增量失败: %v", err)
			} else if log != nil {
				common.SysLog("全市场日线增量完成: %s", log.Message)
			}
			cancel()
			var pending int64
			common.DB.Model(&model.MarketSyncState{}).
				Where("market = ? AND init_status = ?", market, "pending").Count(&pending)
			if pending > 0 {
				if err := svc.StartMarketWideInit(); err == nil {
					common.SysLog("宇宙内尚有 %d 只待建史，已自动启动历史初始化", pending)
				}
			}
		}
		if now := time.Now(); isTradingDayToday(now) && now.Hour()*60+now.Minute() >= 16*60+10 {
			var n int64
			common.DB.Model(&model.DataSyncLog{}).
				Where("task = ? AND status <> ? AND created_at >= ?", "sync_market_wide", "failed",
					now.Format("2006-01-02")+" 00:00:00").Count(&n)
			if n == 0 {
				common.SysLog("启动补跑：今日 16:10 全市场日线增量未执行，现在补跑")
				runWide()
			}
		}
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 16, 10, 0, 0, now.Location())
			if !next.After(now) {
				next = next.AddDate(0, 0, 1)
			}
			time.Sleep(next.Sub(now))
			if !isTradingDayToday(time.Now()) {
				continue
			}
			runWide()
		}
	}()
}
