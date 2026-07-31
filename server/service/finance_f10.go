package service

import (
	"context"
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
	return syncFinanceIndicators(ctx, symbol, false)
}

// syncFinanceIndicators 执行实际同步。force 只用于已有代码证据表明缓存不可用的场景：
// 当前时点没有可用行，或披露日历已确认出现了更晚报告；它不会绕过 1h 尝试冷却。
func syncFinanceIndicators(ctx context.Context, symbol string, force bool) bool {
	if common.DB == nil || !isSixDigits(symbol) {
		return false
	}
	if (!force && finFresh(&model.FinanceIndicator{}, symbol)) || !finTryAllowed("ind:"+symbol) {
		return false
	}
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
		recs = append(recs, model.FinanceIndicator{
			Symbol: symbol, Market: "cn", ReportDate: rd,
			ReportName: truncateRunes(r.String("REPORT_DATE_NAME"), 16),
			NoticeDate: r.Date("NOTICE_DATE"),
			EPS:        r.Float("EPSJB"), BPS: r.Float("BPS"), OCFPS: r.Float("MGJYXJJE"),
			Revenue: r.Float("TOTALOPERATEREVE"), RevenueYoY: r.Float("TOTALOPERATEREVETZ"),
			NetProfit: r.Float("PARENTNETPROFIT"), NetProfitYoY: r.Float("PARENTNETPROFITTZ"),
			DeductProfit: r.Float("KCFJCXSYJLR"), DeductProfitYoY: r.Float("KCFJCXSYJLRTZ"),
			ROE: r.Float("ROEJQ"), GrossMargin: r.Float("XSMLL"), NetMargin: r.Float("XSJLL"),
			DebtRatio: r.Float("ZCFZL"),
		})
	}
	if len(recs) == 0 {
		return true
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "report_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"report_name", "notice_date", "eps", "bps", "ocf_ps",
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
	if common.DB == nil {
		return nil
	}
	var rows []model.FinanceIndicator
	res := common.DB.Where("symbol = ? AND market = ?", symbol, "cn").
		Where("notice_date = '' OR notice_date IS NULL OR notice_date <= ?", asOf).
		Order("report_date DESC, id DESC").Limit(finIndicatorKeep).Find(&rows)
	if res.Error != nil || res.RowsAffected == 0 {
		return nil
	}
	for i := range rows {
		if rows[i].NoticeDate != "" || financeReportPublishedAsOf(symbol, rows[i].ReportDate, asOf) {
			return &rows[i]
		}
	}
	return nil
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
	if finFresh(&model.FinanceStatement{}, symbol) || !finTryAllowed("stmt:"+symbol) {
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
	var inds []model.FinanceIndicator
	common.DB.Where("symbol = ?", symbol).Order("report_date DESC").Limit(finTrendPeriods).Find(&inds)
	var stmts []model.FinanceStatement
	common.DB.Where("symbol = ?", symbol).Order("report_date DESC").Limit(finTrendPeriods).Find(&stmts)
	reverseSlice(inds)
	reverseSlice(stmts)
	return map[string]any{"indicators": inds, "statements": stmts}, nil
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
	var inds []model.FinanceIndicator
	if err := common.DB.Where("symbol = ?", symbol).
		Order("report_date DESC").Limit(finTrendPeriods).Find(&inds).Error; err != nil || len(inds) == 0 {
		return nil
	}
	latest := inds[0]
	brief := map[string]any{
		"report":      latest.ReportName,
		"notice_date": latest.NoticeDate,
		"latest": map[string]any{
			"eps":               round2(latest.EPS),
			"bps":               round2(latest.BPS),
			"ocf_ps":            round2(latest.OCFPS),
			"revenue_yi":        round2(latest.Revenue / 1e8),
			"revenue_yoy":       round2(latest.RevenueYoY),
			"net_profit_yi":     round2(latest.NetProfit / 1e8),
			"net_profit_yoy":    round2(latest.NetProfitYoY),
			"deduct_profit_yoy": round2(latest.DeductProfitYoY),
			"roe":               round2(latest.ROE),
			"gross_margin":      round2(latest.GrossMargin),
			"net_margin":        round2(latest.NetMargin),
			"debt_ratio":        round2(latest.DebtRatio),
		},
		"note": "F10 主要财务指标（东财口径；金额亿元、比率%；0 可能表示上游缺失）。trend 为近几期概要，最早在前",
	}
	trend := make([]map[string]any, 0, len(inds))
	for i := len(inds) - 1; i >= 0; i-- { // 升序
		r := inds[i]
		trend = append(trend, map[string]any{
			"report":         r.ReportName,
			"revenue_yi":     round2(r.Revenue / 1e8),
			"revenue_yoy":    round2(r.RevenueYoY),
			"net_profit_yi":  round2(r.NetProfit / 1e8),
			"net_profit_yoy": round2(r.NetProfitYoY),
			"roe":            round2(r.ROE),
			"gross_margin":   round2(r.GrossMargin),
		})
	}
	brief["trend"] = trend

	// 三表补充（只读缓存，详情页访问过才有）：现金流与资产负债的绝对科目。
	var st model.FinanceStatement
	if err := common.DB.Where("symbol = ?", symbol).Order("report_date DESC").First(&st).Error; err == nil {
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

// candFin 长线推荐候选的财务摘要（进 LLM 名单、核验值域与前端因子面板）。
type candFin struct {
	Report       string  `json:"report"`        // 「2026一季报」
	ROE          float64 `json:"roe,omitempty"` // 加权 ROE %
	RevenueYoY   float64 `json:"revenue_yoy"`   // 营收同比 %（可为负，不 omitempty）
	NetProfitYoY float64 `json:"net_profit_yoy"`
	GrossMargin  float64 `json:"gross_margin,omitempty"`
	NetMargin    float64 `json:"net_margin,omitempty"`
	DebtRatio    float64 `json:"debt_ratio,omitempty"`
}

// financeFactorFor 读取某股截至当前时点可用的最新一期财务摘要供推荐评分/LLM 名单。
// 披露日历确认有更新报告时强制尝试刷新；若刷新失败则 fail-closed，不让已知过期报告
// 继续加分。没有更新事件时仍沿用 7 天同步水位作缺日历的容灾探测。
func financeFactorFor(ctx context.Context, symbol string, budget *int) *candFin {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	asOf := time.Now().In(time.Local).Format("2006-01-02")
	toFactor := func(r *model.FinanceIndicator) *candFin {
		if r == nil {
			return nil
		}
		return &candFin{
			Report: r.ReportName, ROE: round2(r.ROE),
			RevenueYoY: round2(r.RevenueYoY), NetProfitYoY: round2(r.NetProfitYoY),
			GrossMargin: round2(r.GrossMargin), NetMargin: round2(r.NetMargin), DebtRatio: round2(r.DebtRatio),
		}
	}

	cached := financeIndicatorAsOf(symbol, asOf)
	requiredReport := ""
	if cached != nil {
		requiredReport = publishedFinanceReportAfter(symbol, cached.ReportDate, asOf)
	}
	refreshNeeded := cached == nil || requiredReport != "" || !finFresh(&model.FinanceIndicator{}, symbol)
	if !refreshNeeded {
		return toFactor(cached)
	}
	if budget == nil || *budget <= 0 {
		if requiredReport != "" {
			return nil // 已知有新报告但无法刷新，旧报告不得继续参与推荐。
		}
		return toFactor(cached)
	}
	if syncFinanceIndicators(ctx, symbol, cached == nil || requiredReport != "") {
		*budget--
	}
	latest := financeIndicatorAsOf(symbol, asOf)
	if requiredReport != "" && (latest == nil || latest.ReportDate < requiredReport) {
		return nil
	}
	return toFactor(latest)
}
