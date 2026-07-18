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
// 之后走本地缓存；新鲜度 7 天（财报按季披露，7 天再查一次足够及时）。
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
	finSyncMu   sync.Mutex
	finSyncTry  = map[string]time.Time{} // "ind:600519" / "stmt:600519" → 上次尝试时刻
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

// finFresh 表内该股最新 updated_at 是否仍在新鲜期。
func finFresh(mdl any, symbol string) bool {
	if common.DB == nil {
		return true // 无 DB 环境（纯函数单测）不触发上游
	}
	var last time.Time
	row := common.DB.Model(mdl).Where("symbol = ?", symbol).Select("MAX(updated_at)").Row()
	if row == nil || row.Scan(&last) != nil || last.IsZero() {
		return false
	}
	return time.Since(last) < finFreshTTL
}

// ensureFinanceIndicators F10 主要财务指标按需同步（best-effort：失败静默，
// 消费方按「缓存里有什么用什么」处理）。返回是否发生了上游拉取。
func ensureFinanceIndicators(ctx context.Context, symbol string) bool {
	if common.DB == nil || !isSixDigits(symbol) {
		return false
	}
	if finFresh(&model.FinanceIndicator{}, symbol) || !finTryAllowed("ind:"+symbol) {
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

// financeFactorFor 读取某股最新一期财务摘要供推荐评分/LLM 名单。
// DB 缓存优先；缺失且预算未耗尽时上游拉一次（budget 由单次生成共享，控总时长）。
// 返回 nil = 无数据（缺失不惩罚）。
func financeFactorFor(ctx context.Context, symbol string, budget *int) *candFin {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	load := func() *candFin {
		var r model.FinanceIndicator
		if err := common.DB.Where("symbol = ?", symbol).Order("report_date DESC").First(&r).Error; err != nil {
			return nil
		}
		return &candFin{
			Report: r.ReportName, ROE: round2(r.ROE),
			RevenueYoY: round2(r.RevenueYoY), NetProfitYoY: round2(r.NetProfitYoY),
			GrossMargin: round2(r.GrossMargin), NetMargin: round2(r.NetMargin), DebtRatio: round2(r.DebtRatio),
		}
	}
	if f := load(); f != nil {
		return f
	}
	if budget == nil || *budget <= 0 {
		return nil
	}
	if ensureFinanceIndicators(ctx, symbol) {
		*budget--
	}
	return load()
}
