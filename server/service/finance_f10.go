package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// F2 财务数据服务：F10 主要财务指标 + 三大报表关键科目的按需拉取与缓存。
// 不做全市场普查——只有个股详情/AI 快照/长线推荐候选首次访问才触发上游拉取，
// 之后走本地缓存；推荐路径以披露日历的新报告信号触发刷新，7 天水位只作缺日历时
// 的容灾探测（财报按季披露）。
// 冷却/新鲜度状态用包级共享（FinanceService 有多个实例，annFetch 前科）。

var (
	fetchF10        = datasource.GetF10MainFinance // 注入点：单测替换
	fetchStatements = datasource.GetEMStatements
)

const (
	finFreshTTL       = 7 * 24 * time.Hour // 缓存新鲜期：期内不回上游
	finAttemptCool    = time.Hour          // 拉取尝试冷却（成功失败都算，防刷）
	finIndicatorKeep  = 200                // F10 落库期数上限（单请求即 200 期）
	finTrendPeriods   = 8                  // 详情页/AI 上下文取最近 8 期
	finRecFetchBudget = 12                 // 单次长线推荐生成允许的上游 F10 拉取只数
)

var (
	finSyncMu  sync.Mutex
	finSyncTry = map[string]time.Time{} // "ind:600519" / "stmt:600519" → 上次尝试时刻
)

// finTryAllowed 尝试冷却检查（成功失败一律记时刻，1h 内不重试同一目标）。
func finTryAllowed(key string) bool {
	finSyncMu.Lock()
	defer finSyncMu.Unlock()
	if t, ok := finSyncTry[key]; ok && time.Since(t) < finAttemptCool {
		return false
	}
	finSyncTry[key] = time.Now()
	return true
}

// finFresh 表内该股最新 updated_at 是否仍在新鲜期。不要用 MAX(updated_at)：
// SQLite 聚合表达式会丢失列的时间类型，扫描到 time.Time 失败后水位会永久为 false。
func finFresh(mdl any, symbol string) bool {
	if common.DB == nil {
		return true // 无 DB 环境（纯函数单测）不触发上游
	}
	var row struct {
		UpdatedAt time.Time `gorm:"column:updated_at"`
	}
	res := common.DB.Model(mdl).Where("symbol = ?", symbol).
		Select("updated_at").Order("updated_at DESC").Limit(1).Scan(&row)
	if res.Error != nil || res.RowsAffected == 0 || row.UpdatedAt.IsZero() {
		return false
	}
	return time.Since(row.UpdatedAt) < finFreshTTL
}

// ensureFinanceIndicators F10 主要财务指标按需同步（best-effort：失败静默，
// 消费方按「缓存里有什么用什么」处理）。返回是否发生了上游拉取。
func ensureFinanceIndicators(ctx context.Context, symbol string) bool {
	if common.DB == nil || !isSixDigits(symbol) {
		return false
	}
	now := time.Now()
	probe := inspectFinanceFactor(symbol, now.In(time.Local).Format("2006-01-02"), now)
	if !probe.RefreshNeeded {
		return false
	}
	return syncFinanceIndicators(ctx, symbol, probe.RefreshNeeded)
}

// syncFinanceIndicators 执行实际同步。force 只用于已有代码证据表明缓存不可用的场景：
// 当前时点没有可用行、实际选中行过期，或披露日历已确认出现更晚报告；不会绕过 1h 冷却。
func syncFinanceIndicators(ctx context.Context, symbol string, force bool) bool {
	if common.DB == nil || !isSixDigits(symbol) {
		return false
	}
	if (!force && finFresh(&model.FinanceIndicator{}, symbol)) || !finTryAllowed("ind:"+symbol) {
		return false
	}
	return fetchFinanceIndicators(ctx, symbol)
}

// fetchFinanceIndicators 执行一次已经通过新鲜度/冷却规划的真实上游请求。
// 返回 true 表示请求已发出；请求失败、空响应或落库失败仍属于一次预算消耗。
func fetchFinanceIndicators(ctx context.Context, symbol string) bool {
	rows, err := fetchF10(ctx, symbol)
	if err != nil {
		common.SysDebug("F10 财务指标拉取失败 %s: %v", symbol, err)
		return true
	}
	if len(rows) > finIndicatorKeep {
		rows = rows[:finIndicatorKeep]
	}
	recs := make([]model.FinanceIndicator, 0, len(rows))
	for _, r := range rows {
		rd := r.Date("REPORT_DATE")
		if rd == "" {
			continue
		}
		rec := model.FinanceIndicator{
			Symbol: symbol, Market: "cn", ReportDate: rd,
			ReportName: truncateRunes(r.String("REPORT_DATE_NAME"), 16),
			NoticeDate: r.Date("NOTICE_DATE"),
			ValueMask:  model.FinanceFieldsKnown,
		}
		for _, f := range []struct {
			key   string
			mask  uint32
			value *float64
		}{
			{"EPSJB", model.FinanceFieldEPS, &rec.EPS}, {"BPS", model.FinanceFieldBPS, &rec.BPS},
			{"MGJYXJJE", model.FinanceFieldOCFPS, &rec.OCFPS},
			{"TOTALOPERATEREVE", model.FinanceFieldRevenue, &rec.Revenue}, {"TOTALOPERATEREVETZ", model.FinanceFieldRevenueYoY, &rec.RevenueYoY},
			{"PARENTNETPROFIT", model.FinanceFieldNetProfit, &rec.NetProfit}, {"PARENTNETPROFITTZ", model.FinanceFieldNetProfitYoY, &rec.NetProfitYoY},
			{"KCFJCXSYJLR", model.FinanceFieldDeductProfit, &rec.DeductProfit}, {"KCFJCXSYJLRTZ", model.FinanceFieldDeductProfitYoY, &rec.DeductProfitYoY},
			{"ROEJQ", model.FinanceFieldROE, &rec.ROE}, {"XSMLL", model.FinanceFieldGrossMargin, &rec.GrossMargin},
			{"XSJLL", model.FinanceFieldNetMargin, &rec.NetMargin}, {"ZCFZL", model.FinanceFieldDebtRatio, &rec.DebtRatio},
		} {
			if value, ok := r.FloatOK(f.key); ok {
				*f.value = value
				rec.ValueMask |= f.mask
			}
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return true
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "report_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"report_name", "notice_date", "value_mask", "eps", "bps", "ocf_ps",
			"revenue", "revenue_yoy", "net_profit", "net_profit_yoy", "deduct_profit", "deduct_profit_yoy",
			"roe", "gross_margin", "net_margin", "debt_ratio", "updated_at"}),
	}).CreateInBatches(recs, 100).Error; err != nil {
		common.SysWarn("财务指标落库失败 %s: %v", symbol, err)
	}
	return true
}

// financeIndicatorAsOf 返回截至 asOf 可证明已公告的最新 F10 行。NoticeDate 是首选
// 可用时点；上游缺公告日时，必须由披露日历证明同报告期已实际发布，不能仅凭较新的
// ReportDate 猜测可用性。
func financeIndicatorAsOf(symbol, asOf string) *model.FinanceIndicator {
	rows, err := readFinanceIndicatorsAsOf(context.Background(), symbol, asOf, 1)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// 推荐、详情与 AI 快照共用同一披露证据筛选，并在 LIMIT 前排除未披露行。
func readFinanceIndicatorsAsOf(ctx context.Context, symbol, asOf string, limit int) ([]model.FinanceIndicator, error) {
	rows := []model.FinanceIndicator{}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	err := common.DB.WithContext(ctx).Where("symbol = ? AND market = ? AND report_date <= ?", symbol, "cn", asOf).
		Where(`(notice_date <> '' AND notice_date <= ?) OR
			((notice_date = '' OR notice_date IS NULL) AND EXISTS (
				SELECT 1 FROM disclosure_schedules ds
				WHERE ds.symbol = finance_indicators.symbol AND ds.market = finance_indicators.market
				AND ds.report_date = finance_indicators.report_date
				AND (`+financePublishedAsOfClause+`)))`, asOf, asOf, true, asOf).
		Order("report_date DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// 三表没有公告日期，只交付同报告期已有披露证据的科目，避免新旧口径错配。
func readFinanceStatementsForIndicators(ctx context.Context, symbol string, indicators []model.FinanceIndicator) ([]model.FinanceStatement, error) {
	rows := []model.FinanceStatement{}
	dates := make([]string, 0, len(indicators))
	for _, row := range indicators {
		dates = append(dates, row.ReportDate)
	}
	if len(dates) == 0 {
		return rows, nil
	}
	err := common.DB.WithContext(ctx).Where("symbol = ? AND market = ? AND report_date IN ?", symbol, "cn", dates).
		Order("report_date DESC, id DESC").Limit(finTrendPeriods).Find(&rows).Error
	return rows, err
}

const financePublishedAsOfClause = `(actual_date <> '' AND actual_date <= ?)
	OR ((actual_date = '' OR actual_date IS NULL) AND is_published = ?
		AND appoint_date <> '' AND appoint_date <= ?)`

func financeReportPublishedAsOf(symbol, reportDate, asOf string) bool {
	if common.DB == nil || reportDate == "" {
		return false
	}
	var count int64
	res := common.DB.Model(&model.DisclosureSchedule{}).
		Where("symbol = ? AND market = ? AND report_date = ?", symbol, "cn", reportDate).
		Where(financePublishedAsOfClause, asOf, true, asOf).Count(&count)
	return res.Error == nil && count > 0
}

// publishedFinanceReportAfter 用每日刷新的披露日历判断缓存是否已明确落后一季。
// ActualDate 优先；上游缺 ActualDate 时才以 IsPublished+已到预约日兜底。所有日期都
// 截断在 asOf，避免未来预约/公告造成 point-in-time 泄漏。
func publishedFinanceReportAfter(symbol, reportDate, asOf string) string {
	if common.DB == nil || reportDate == "" {
		return ""
	}
	var row model.DisclosureSchedule
	res := common.DB.Where("symbol = ? AND market = ? AND report_date > ?", symbol, "cn", reportDate).
		Where(financePublishedAsOfClause, asOf, true, asOf).
		Order("report_date DESC, id DESC").Limit(1).Find(&row)
	if res.Error != nil || res.RowsAffected == 0 {
		return ""
	}
	return row.ReportDate
}

// ensureFinanceStatements 三大报表关键科目按需同步（约 7 次上游请求 ≈3~4s，
// 只在个股详情财务块访问时触发，AI 快照与推荐不触发）。
func ensureFinanceStatements(ctx context.Context, symbol string) {
	if common.DB == nil || !isSixDigits(symbol) {
		return
	}
	fresh := finFresh(&model.FinanceStatement{}, symbol)
	if fresh {
		var latest model.FinanceStatement
		err := common.DB.WithContext(ctx).Where("symbol = ? AND market = ?", symbol, "cn").
			Order("report_date DESC").First(&latest).Error
		asOf := time.Now().In(time.Local).Format("2006-01-02")
		indicator := financeIndicatorAsOf(symbol, asOf)
		fresh = err == nil && publishedFinanceReportAfter(symbol, latest.ReportDate, asOf) == "" &&
			(indicator == nil || indicator.ReportDate <= latest.ReportDate)
	}
	if fresh || !finTryAllowed("stmt:"+symbol) {
		return
	}
	rows, err := fetchStatements(ctx, symbol)
	if err != nil {
		common.SysDebug("三大报表拉取失败 %s: %v", symbol, err)
		return
	}
	recs := make([]model.FinanceStatement, 0, len(rows))
	for _, r := range rows {
		if r.ReportDate == "" {
			continue
		}
		recs = append(recs, model.FinanceStatement{
			Symbol: symbol, Market: "cn", ReportDate: r.ReportDate,
			MonetaryFunds: r.MonetaryFunds, AccountsRece: r.AccountsRece, Inventory: r.Inventory,
			TotalAssets: r.TotalAssets, TotalLiabilities: r.TotalLiabilities, TotalEquity: r.TotalEquity,
			OperateIncome: r.OperateIncome, OperateCost: r.OperateCost, OperateProfit: r.OperateProfit,
			RDExpense: r.RDExpense, NetcashOperate: r.NetcashOperate, NetcashInvest: r.NetcashInvest,
			NetcashFinance: r.NetcashFinance,
		})
	}
	if len(recs) == 0 {
		return
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "report_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"monetary_funds", "accounts_rece", "inventory",
			"total_assets", "total_liabilities", "total_equity", "operate_income", "operate_cost",
			"operate_profit", "rd_expense", "netcash_operate", "netcash_invest", "netcash_finance", "updated_at"}),
	}).CreateInBatches(recs, 50).Error; err != nil {
		common.SysWarn("三大报表落库失败 %s: %v", symbol, err)
	}
}

// FinanceOverview 详情页财务块：最近 8 期指标与报表科目（升序，图表直接可用）。
// 首次访问触发按需同步（F10 一次请求 + 三表约 7 次，冷却 1h）。
func (s *FinanceService) FinanceOverview(ctx context.Context, symbol string) (map[string]any, error) {
	symbol = strings.TrimSpace(symbol)
	if !isSixDigits(symbol) {
		return map[string]any{"indicators": []model.FinanceIndicator{}, "statements": []model.FinanceStatement{}}, nil
	}
	ensureFinanceIndicators(ctx, symbol)
	ensureFinanceStatements(ctx, symbol)
	now := time.Now()
	asOf := now.In(time.Local).Format("2006-01-02")
	inds, err := readFinanceIndicatorsAsOf(ctx, symbol, asOf, finTrendPeriods)
	if err != nil {
		return nil, err
	}
	stmts, err := readFinanceStatementsForIndicators(ctx, symbol, inds)
	if err != nil {
		return nil, err
	}
	note := ""
	if len(inds) == 0 {
		note = "尚未读取到有披露依据的财务指标，数据缺失不代表没有财报"
	} else if !financeIndicatorRowFreshAt(&inds[0], now) || publishedFinanceReportAfter(symbol, inds[0].ReportDate, asOf) != "" {
		note = "财务缓存尚未更新，以下仅展示已确认披露的历史数据，请核对报告期"
	}
	reverseSlice(inds)
	reverseSlice(stmts)
	return map[string]any{"indicators": inds, "statements": stmts, "note": note}, nil
}

func reverseSlice[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// financeBrief 个股 AI 快照的财务段（分析/问答共用）：最新一期关键指标 + 近 8 期趋势。
// F10 缓存缺失时按需拉一次（单请求，interactive 路径可承受）；三表科目只读缓存。
// 无数据返回 nil（prompt 已声明缺失时如实说明）。数值经 round2 后是 JSON 数值叶子，
// snapshotLabeledValues 会自动并入证据核验值域。
func financeBrief(ctx context.Context, symbol string) map[string]any {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	ensureFinanceIndicators(ctx, symbol)
	now := time.Now()
	asOf := now.In(time.Local).Format("2006-01-02")
	inds, err := readFinanceIndicatorsAsOf(ctx, symbol, asOf, finTrendPeriods)
	if err != nil || len(inds) == 0 {
		return nil
	}
	latest := inds[0]
	if !financeIndicatorRowFreshAt(&latest, now) || publishedFinanceReportAfter(symbol, latest.ReportDate, asOf) != "" {
		return nil
	}
	brief := map[string]any{
		"version":     financeFactorVersion,
		"report":      latest.ReportName,
		"report_date": latest.ReportDate,
		"notice_date": latest.NoticeDate,
		"latest": map[string]any{
			"eps":               financeBriefValue(latest, model.FinanceFieldEPS, latest.EPS, 1),
			"bps":               financeBriefValue(latest, model.FinanceFieldBPS, latest.BPS, 1),
			"ocf_ps":            financeBriefValue(latest, model.FinanceFieldOCFPS, latest.OCFPS, 1),
			"revenue_yi":        financeBriefValue(latest, model.FinanceFieldRevenue, latest.Revenue, 1e8),
			"revenue_yoy":       financeBriefValue(latest, model.FinanceFieldRevenueYoY, latest.RevenueYoY, 1),
			"net_profit_yi":     financeBriefValue(latest, model.FinanceFieldNetProfit, latest.NetProfit, 1e8),
			"net_profit_yoy":    financeBriefValue(latest, model.FinanceFieldNetProfitYoY, latest.NetProfitYoY, 1),
			"deduct_profit_yoy": financeBriefValue(latest, model.FinanceFieldDeductProfitYoY, latest.DeductProfitYoY, 1),
			"roe":               financeBriefValue(latest, model.FinanceFieldROE, latest.ROE, 1),
			"gross_margin":      financeBriefValue(latest, model.FinanceFieldGrossMargin, latest.GrossMargin, 1),
			"net_margin":        financeBriefValue(latest, model.FinanceFieldNetMargin, latest.NetMargin, 1),
			"debt_ratio":        financeBriefValue(latest, model.FinanceFieldDebtRatio, latest.DebtRatio, 1),
		},
		"note": "F10 主要指标 latest/trend/annual 使用可空数值（金额亿元、比率%；null/省略表示缺失，数值0表示已知为零）。roe 是本报告期累计ROE，不能把季度值与年度阈值直接比较；年度质量参照见 annual。trend 最早在前，同期可比后再判断变化；statement_latest 沿用旧三表缓存，其0值不能证明归零",
	}
	annual := latestAnnualFinanceIndicator(inds)
	if financeIndicatorRowFreshAt(annual, now) && publishedAnnualFinanceAfter(symbol, annual, asOf) == "" {
		brief["annual"] = map[string]any{
			"report_date": annual.ReportDate, "notice_date": annual.NoticeDate,
			"roe": financeBriefValue(*annual, model.FinanceFieldROE, annual.ROE, 1),
		}
	}
	trend := make([]map[string]any, 0, len(inds))
	for i := len(inds) - 1; i >= 0; i-- { // 升序
		r := inds[i]
		trend = append(trend, map[string]any{
			"report":         r.ReportName,
			"report_date":    r.ReportDate,
			"notice_date":    r.NoticeDate,
			"revenue_yi":     financeBriefValue(r, model.FinanceFieldRevenue, r.Revenue, 1e8),
			"revenue_yoy":    financeBriefValue(r, model.FinanceFieldRevenueYoY, r.RevenueYoY, 1),
			"net_profit_yi":  financeBriefValue(r, model.FinanceFieldNetProfit, r.NetProfit, 1e8),
			"net_profit_yoy": financeBriefValue(r, model.FinanceFieldNetProfitYoY, r.NetProfitYoY, 1),
			"roe":            financeBriefValue(r, model.FinanceFieldROE, r.ROE, 1),
			"gross_margin":   financeBriefValue(r, model.FinanceFieldGrossMargin, r.GrossMargin, 1),
		})
	}
	brief["trend"] = trend

	// 三表补充（只读缓存，详情页访问过才有）：现金流与资产负债的绝对科目。
	if statements, err := readFinanceStatementsForIndicators(ctx, symbol, inds); err == nil && len(statements) > 0 {
		st := statements[0]
		brief["statement_latest"] = map[string]any{
			"report_date":        st.ReportDate,
			"monetary_funds_yi":  round2(st.MonetaryFunds / 1e8),
			"inventory_yi":       round2(st.Inventory / 1e8),
			"total_assets_yi":    round2(st.TotalAssets / 1e8),
			"netcash_operate_yi": round2(st.NetcashOperate / 1e8),
			"netcash_invest_yi":  round2(st.NetcashInvest / 1e8),
			"rd_expense_yi":      round2(st.RDExpense / 1e8),
		}
	}
	return brief
}

func financeBriefValue(row model.FinanceIndicator, field uint32, value, divisor float64) any {
	if row.OptionalValue(field, value) == nil {
		return nil
	}
	return round2(value / divisor)
}

const financeFactorVersion = "ff1"

// candFin 同比使用最新已披露报告；年度质量使用最近已披露年报的加权 ROE。
// nil 是缺失，ff1 的非 nil 零值是确实披露的 0；旧摘要零值仍保持未知。
type candFin struct {
	Version          string   `json:"version,omitempty"`
	Report           string   `json:"report"`
	ReportDate       string   `json:"report_date,omitempty"`
	NoticeDate       string   `json:"notice_date,omitempty"`
	ROE              *float64 `json:"roe,omitempty"` // 本报告期累计加权 ROE，不作年化阈值比较
	RevenueYoY       *float64 `json:"revenue_yoy,omitempty"`
	NetProfitYoY     *float64 `json:"net_profit_yoy,omitempty"`
	GrossMargin      *float64 `json:"gross_margin,omitempty"`
	NetMargin        *float64 `json:"net_margin,omitempty"`
	DebtRatio        *float64 `json:"debt_ratio,omitempty"`
	AnnualROE        *float64 `json:"annual_roe,omitempty"`
	AnnualReportDate string   `json:"annual_report_date,omitempty"`
	AnnualNoticeDate string   `json:"annual_notice_date,omitempty"`
}

func (f *candFin) has(value *float64) bool {
	return f != nil && value != nil && finiteRecNumber(*value) && (f.Version == financeFactorVersion || *value != 0)
}

func (f *candFin) hasAnnualROE() bool {
	return f != nil && f.has(f.AnnualROE) && strings.HasSuffix(f.AnnualReportDate, "-12-31")
}

func financeMissingFor(profile string, f *candFin) []string {
	if !profileUsesFinance(profile) {
		return nil
	}
	if f == nil {
		return []string{"有效财报摘要"}
	}
	var missing []string
	if !f.hasAnnualROE() {
		missing = append(missing, "最近已披露年报 ROE")
	}
	if !f.has(f.NetProfitYoY) {
		missing = append(missing, "最新报告净利润同比")
	}
	if profile == "growth" && !f.has(f.RevenueYoY) {
		missing = append(missing, "最新报告营收同比")
	}
	return missing
}

// financeFactorProbe 是推荐轮在任何补拉发生前读取并冻结的本地财务状态。
// RequiredReport 非空表示披露日历已证明缓存落后一季；Fresh 表示探测时缓存仍在 TTL 内。
// 任一 stale 状态刷新未成功都必须 fail-closed。
type financeFactorProbe struct {
	Symbol         string
	AsOf           string
	Cached         *model.FinanceIndicator
	CachedAnnual   *model.FinanceIndicator
	RequiredReport string
	RequiredAnnual string
	Fresh          bool
	AnnualFresh    bool
	RefreshNeeded  bool
}

// financeIndicatorRowFreshAt 必须检查真正被 point-in-time 选择的那一行，不能用
// 同标的其他（可能尚未披露或无法证明可用的）行的 updated_at 代替。
func financeIndicatorRowFreshAt(row *model.FinanceIndicator, now time.Time) bool {
	if row == nil || row.UpdatedAt.IsZero() || row.UpdatedAt.After(now.Add(time.Minute)) {
		return false
	}
	return now.Sub(row.UpdatedAt) < finFreshTTL
}

func inspectFinanceFactor(symbol, asOf string, now time.Time) financeFactorProbe {
	p := financeFactorProbe{Symbol: symbol, AsOf: asOf}
	if common.DB == nil || !isSixDigits(symbol) {
		return p
	}
	rows, err := readFinanceIndicatorsAsOf(context.Background(), symbol, asOf, finTrendPeriods)
	if err == nil && len(rows) > 0 {
		p.Cached = &rows[0]
		p.CachedAnnual = latestAnnualFinanceIndicator(rows)
	}
	if p.Cached != nil {
		p.RequiredReport = publishedFinanceReportAfter(symbol, p.Cached.ReportDate, asOf)
		p.Fresh = financeIndicatorRowFreshAt(p.Cached, now)
		p.RequiredAnnual = publishedAnnualFinanceAfter(symbol, p.CachedAnnual, asOf)
		p.AnnualFresh = financeIndicatorRowFreshAt(p.CachedAnnual, now)
	}
	p.RefreshNeeded = p.Cached == nil || p.RequiredReport != "" || !p.Fresh ||
		p.RequiredAnnual != "" || (p.CachedAnnual != nil && !p.AnnualFresh)
	return p
}

func latestAnnualFinanceIndicator(rows []model.FinanceIndicator) *model.FinanceIndicator {
	// 先选最近年报，再判断字段是否可用；不能跳过缺失行去挑一份更好看的旧年报。
	for i := range rows {
		if strings.HasSuffix(rows[i].ReportDate, "-12-31") {
			return &rows[i]
		}
	}
	return nil
}

func publishedAnnualFinanceAfter(symbol string, cached *model.FinanceIndicator, asOf string) string {
	after := ""
	if cached != nil {
		after = cached.ReportDate
	}
	var row model.DisclosureSchedule
	res := common.DB.Where("symbol = ? AND market = ? AND report_date > ? AND report_date <= ? AND report_date LIKE ?", symbol, "cn", after, asOf, "%-12-31").
		Where(financePublishedAsOfClause, asOf, true, asOf).
		Order("report_date DESC, id DESC").Limit(1).Find(&row)
	if res.Error != nil || res.RowsAffected == 0 {
		return ""
	}
	return row.ReportDate
}

func financeValue(r *model.FinanceIndicator, field uint32, value float64) *float64 {
	if r == nil || r.OptionalValue(field, value) == nil {
		return nil
	}
	return recNumber(round2(value))
}

func financeIndicatorToFactor(r, annual *model.FinanceIndicator) *candFin {
	if r == nil {
		return nil
	}
	f := &candFin{
		Version: financeFactorVersion, Report: r.ReportName, ReportDate: r.ReportDate, NoticeDate: r.NoticeDate,
		ROE:        financeValue(r, model.FinanceFieldROE, r.ROE),
		RevenueYoY: financeValue(r, model.FinanceFieldRevenueYoY, r.RevenueYoY), NetProfitYoY: financeValue(r, model.FinanceFieldNetProfitYoY, r.NetProfitYoY),
		GrossMargin: financeValue(r, model.FinanceFieldGrossMargin, r.GrossMargin),
		NetMargin:   financeValue(r, model.FinanceFieldNetMargin, r.NetMargin), DebtRatio: financeValue(r, model.FinanceFieldDebtRatio, r.DebtRatio),
	}
	if annual != nil {
		f.AnnualROE = financeValue(annual, model.FinanceFieldROE, annual.ROE)
		f.AnnualReportDate, f.AnnualNoticeDate = annual.ReportDate, annual.NoticeDate
	}
	return f
}

// resolveFinanceFactor 把探测时状态与补拉结果冻结成推荐可消费的因子。
// fetched=false 时绝不重读 DB，避免其他并发写入改变已规划轮次的事实集合。
func resolveFinanceFactor(p financeFactorProbe, fetched bool) *candFin {
	latest := p.Cached
	annual := p.CachedAnnual
	fresh := p.Fresh
	annualFresh := p.AnnualFresh
	if fetched {
		rows, err := readFinanceIndicatorsAsOf(context.Background(), p.Symbol, p.AsOf, finTrendPeriods)
		if err != nil || len(rows) == 0 {
			return nil
		}
		latest, annual = &rows[0], latestAnnualFinanceIndicator(rows)
		fresh = financeIndicatorRowFreshAt(latest, time.Now())
		annualFresh = financeIndicatorRowFreshAt(annual, time.Now())
	}
	if !fresh || (p.RequiredReport != "" && (latest == nil || latest.ReportDate < p.RequiredReport)) {
		return nil
	}
	if !annualFresh || (p.RequiredAnnual != "" && (annual == nil || annual.ReportDate < p.RequiredAnnual)) {
		annual = nil
	}
	return financeIndicatorToFactor(latest, annual)
}

// financeFactorFor 读取某股截至当前时点可用的最新一期财务摘要供推荐评分/LLM 名单。
// 披露日历确认有更新报告或缓存超过 7 天 TTL 时尝试刷新；若刷新未成功则
// fail-closed，不让 stale 报告继续参与推荐。
func financeFactorFor(ctx context.Context, symbol string, budget *int) *candFin {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	now := time.Now()
	asOf := now.In(time.Local).Format("2006-01-02")
	probe := inspectFinanceFactor(symbol, asOf, now)
	if !probe.RefreshNeeded {
		return resolveFinanceFactor(probe, false)
	}
	if budget == nil || *budget <= 0 {
		return resolveFinanceFactor(probe, false)
	}
	fetched := syncFinanceIndicators(ctx, symbol, probe.RefreshNeeded)
	if fetched {
		*budget--
	}
	return resolveFinanceFactor(probe, fetched)
}
