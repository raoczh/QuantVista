package service

import (
	"time"

	"quantvista/common"
	"quantvista/model"
)

// C10 股息率（第六十二批）。
//
// 来源：`corporate_actions.dividend_yield`（B8 随分红报表 RPT_SHAREBONUS_DET.DIVIDENT_RATIO
// 落库，上游小数已 ×100，单位=百分比数值）。本文件是全项目**唯一**的「这只票的股息率是多少」
// 口径实现——个股详情估值区、AI 个股快照证据链、选股因子宽表三个落点共用，
// 避免三处各挑一行算出三个不同的数字。
//
// 口径纪律：
//   - **只认 dividend_yield > 0 的方案行**：上游对预案（未确定分配额）与纯送转方案不给股息率，
//     落库为 0——0 是「这条方案没有股息率」而不是「股息率为零」，绝不能被选中当答案；
//   - **只回看 dividendYieldMaxAgeDays 天内的报告期**：股息率是滚动年度口径，
//     三年前的年报股息率对今天没有意义，宁可如实缺失也不给过期数字；
//   - 同一只票有多期方案时取**报告期最新**的一期（同报告期再按除权日降序），
//     并把该报告期作为 as-of 一并透出——消费方展示/引用时必须带时点，
//     否则「股息率 3.2%」会被读成实时值；
//   - **缺失就是缺失**：查不到 → 返回 ok=false，调用方一律缺席处理（前端不渲染、
//     因子列 NaN、AI 快照不出该键），不得回退 0（0 会被读成「不分红」）。

// dividendYieldMaxAgeDays 报告期回看窗口（自然日）：超过约两年的方案不再作为「当前股息率」。
// 取 800 天（≈2.2 年）而非 365 天的理由：年报分红方案的报告期是上一年 12-31，
// 而实施往往在次年年中，卡 365 天会把「刚实施完的上年度分红」判成过期。
const dividendYieldMaxAgeDays = 800

// DividendYieldView 一只票的当前股息率与其时点（nil = 无数据，不是 0%）。
type DividendYieldView struct {
	YieldPct    float64 `json:"yield_pct"`              // 股息率 %（>0）
	ReportDate  string  `json:"report_date"`            // 方案报告期（as-of，展示与引用必须带）
	ExDate      string  `json:"ex_date,omitempty"`      // 除权除息日（未定为空）
	Progress    string  `json:"progress,omitempty"`     // 方案进度（实施分配 / 董事会预案…）
	PlanProfile string  `json:"plan_profile,omitempty"` // 方案描述原文
	Note        string  `json:"note"`                   // 口径声明（前端 tooltip / AI 引用时点）
}

// pickLatestDividendYield 从一组分红方案里挑出「当前股息率」。
//
// now 用于计算报告期回看窗口（可测；调用方传 time.Now()）。actions 不要求有序。
// 未命中返回 nil——**调用方必须按缺失处理，不得当 0 用**。
func pickLatestDividendYield(actions []model.CorporateAction, now time.Time) *DividendYieldView {
	cutoff := now.AddDate(0, 0, -dividendYieldMaxAgeDays).Format("2006-01-02")
	var best *model.CorporateAction
	for i := range actions {
		a := &actions[i]
		if a.DividendYield <= 0 {
			continue // 0 = 该方案没有股息率（预案/纯送转），不是「股息率为零」
		}
		if a.ReportDate == "" || a.ReportDate < cutoff {
			continue // 报告期缺失或过期：宁缺毋滥
		}
		if best == nil || a.ReportDate > best.ReportDate ||
			(a.ReportDate == best.ReportDate && a.ExDate > best.ExDate) {
			best = a
		}
	}
	if best == nil {
		return nil
	}
	return &DividendYieldView{
		YieldPct:    round2(best.DividendYield),
		ReportDate:  best.ReportDate,
		ExDate:      best.ExDate,
		Progress:    best.Progress,
		PlanProfile: best.PlanProfile,
		Note: "股息率来自东财分红方案（报告期 " + best.ReportDate +
			"），为该期方案的年度口径，不是按当前股价实时折算的值；引用须带报告期。",
	}
}

// DividendYieldsFor 批量取一组 A 股的当前股息率（因子宽表用）。
//
// 一次查询把窗口内所有有股息率的方案行读进来再按 symbol 归并——全市场约 5000 只 ×
// 窗口内数期方案，量级 2 万行以内，与宽表构建的其它元数据读取同级。
// symbols 为空表示**全市场**（宽表构建走这条路，不需要先知道行数）。
// 返回 map 只含有值的 symbol：**不在 map 里 = 无数据**，与「股息率 0」严格区分。
func DividendYieldsFor(symbols []string, now time.Time) map[string]float64 {
	out := map[string]float64{}
	if common.DB == nil {
		return out
	}
	cutoff := now.AddDate(0, 0, -dividendYieldMaxAgeDays).Format("2006-01-02")
	q := common.DB.Model(&model.CorporateAction{}).
		Select("symbol", "report_date", "ex_date", "dividend_yield").
		Where("market = ? AND dividend_yield > ? AND report_date >= ?", "cn", 0, cutoff)
	if len(symbols) > 0 {
		q = q.Where("symbol IN ?", symbols)
	}
	var rows []model.CorporateAction
	if err := q.Find(&rows).Error; err != nil {
		common.SysWarn("股息率批量查询失败: %v", err)
		return out
	}
	bySymbol := map[string][]model.CorporateAction{}
	for _, r := range rows {
		bySymbol[r.Symbol] = append(bySymbol[r.Symbol], r)
	}
	for sym, list := range bySymbol {
		if v := pickLatestDividendYield(list, now); v != nil {
			out[sym] = v.YieldPct
		}
	}
	return out
}
