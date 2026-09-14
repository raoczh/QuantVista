package service

import (
	"strings"
	"time"

	"quantvista/model"
)

// 盈利性质只由已披露的金额判断，不从同比倒推上年金额：亏损收窄、扭亏和
// 正盈利基期增长是不同事实。现金流每股与加权 EPS 分母不同，不拼成现金转换率。
type earningsEvidence struct {
	Version string   `json:"version"`
	Status  string   `json:"status"`
	Notes   []string `json:"notes,omitempty"`
}

func comparableFinanceReportDate(report string) string {
	d, err := time.Parse("2006-01-02", report)
	if err != nil {
		return ""
	}
	prior := d.AddDate(-1, 0, 0)
	if prior.Month() != d.Month() || prior.Day() != d.Day() {
		return ""
	}
	return prior.Format("2006-01-02")
}

func comparableFinanceIndicator(rows []model.FinanceIndicator, report string) *model.FinanceIndicator {
	want := comparableFinanceReportDate(report)
	if want == "" {
		return nil
	}
	for i := range rows {
		if rows[i].ReportDate == want {
			return &rows[i]
		}
	}
	return nil
}

func (f *candFin) hasComparableProfit() bool {
	return f != nil && f.PriorReportDate != "" && f.PriorReportDate == comparableFinanceReportDate(f.ReportDate) && f.has(f.PriorNetProfit)
}

func (f *candFin) hasPositiveEarnings() bool {
	return f != nil && f.has(f.NetProfit) && *f.NetProfit > 0 && f.has(f.DeductProfit) && *f.DeductProfit > 0
}

func (f *candFin) hasPositiveBaseGrowth() bool {
	return f.hasPositiveEarnings() && f.hasComparableProfit() && *f.PriorNetProfit > 0 &&
		*f.NetProfit > *f.PriorNetProfit && f.has(f.NetProfitYoY) && *f.NetProfitYoY > 0
}

func earningsEvidenceFor(f *candFin) *earningsEvidence {
	e := &earningsEvidence{Version: "fq1", Status: "unknown"}
	if f == nil || !f.has(f.NetProfit) {
		e.Notes = []string{"缺少最新报告归母净利润金额，不能仅凭同比判断盈利"}
		return e
	}
	switch {
	case *f.NetProfit < 0:
		e.Status = "loss_making"
		e.Notes = append(e.Notes, "最新报告归母净利润为负；正同比也可能只是亏损收窄")
	case *f.NetProfit == 0:
		e.Status = "break_even"
		e.Notes = append(e.Notes, "最新报告归母净利润为零，不属于正盈利增长")
	case !f.has(f.DeductProfit):
		e.Status = "core_unknown"
		e.Notes = append(e.Notes, "归母净利润为正，但缺少扣非净利润金额，主营盈利尚待核对")
	case *f.DeductProfit <= 0:
		e.Status = "non_core_profit"
		e.Notes = append(e.Notes, "归母净利润为正但扣非净利润不为正，不能视作已验证的主营盈利质量")
	case !f.hasComparableProfit():
		e.Status = "base_unknown"
		e.Notes = append(e.Notes, "归母与扣非净利润为正，但缺少上年同期金额，不能区分扭亏与正盈利基期增长")
	case *f.PriorNetProfit < 0:
		e.Status = "turnaround"
		e.Notes = append(e.Notes, "上年同期亏损、当期盈利，属于扭亏，不能等同于持续盈利成长")
	case *f.PriorNetProfit == 0:
		e.Status = "zero_base"
		e.Notes = append(e.Notes, "上年同期归母净利润为零，增长率没有可比的正盈利基期")
	case f.hasPositiveBaseGrowth():
		e.Status = "positive_base_growth"
		e.Notes = append(e.Notes, "归母与扣非净利润为正，归母净利润较上年同期正盈利基期增长；单期不能证明长期持续性")
	default:
		e.Status = "profitable"
		e.Notes = append(e.Notes, "归母与扣非净利润为正，但当前证据未同时验证正盈利基期增长")
	}
	if f.has(f.OCFPS) && *f.OCFPS < 0 {
		e.Notes = append(e.Notes, "本报告期每股经营现金流为负，需结合季节性及应收存货核查；不能据此单独认定造假或经营恶化")
	}
	return e
}

func financeEntryIssues(profile string, f *candFin) []string {
	if !profileUsesFinance(profile) || f == nil {
		return nil
	}
	var issues []string
	if f.has(f.NetProfit) && *f.NetProfit <= 0 {
		issues = append(issues, "最新报告归母净利润不为正，盈利质量策略等待业绩验证")
	}
	if f.has(f.DeductProfit) && *f.DeductProfit <= 0 {
		issues = append(issues, "最新报告扣非净利润不为正，盈利质量策略等待主营盈利验证")
	}
	if profile == "growth" && f.hasComparableProfit() && *f.PriorNetProfit <= 0 {
		issues = append(issues, "成长策略的上年同期盈利基期不为正，扭亏或零基数机会先观察")
	}
	if profile == "growth" && f.hasComparableProfit() && f.has(f.NetProfit) && f.has(f.NetProfitYoY) &&
		*f.PriorNetProfit > 0 && *f.NetProfit <= *f.PriorNetProfit && *f.NetProfitYoY > 0 {
		issues = append(issues, "报告同比与缓存同期金额方向不一致，需核对财报重述或口径变化后再验证成长")
	}
	return issues
}

// 独立于可自定义任务段，供个股、推荐、问答和复核共享；不要求增加输出字段。
const earningsPromptDiscipline = `盈利证据纪律：先看归母/扣非净利润金额，再看同比；同比为正可能只是亏损收窄、扭亏或零基数，不能直接称为盈利成长。earnings.status 是程序按披露金额给出的分类，不是胜率或收益预测。比较必须用同一报告季的上年同期，不能把一季与半年/全年累计金额直接比较；有重述或口径冲突时说明缺口。年度 ROE 仅用已披露年报。每股经营现金流与加权 EPS 的分母可能不同，不能直接相除冒充现金转换率；未提供完整同口径三表时不能断言现金流质量、造假或持续成长。`

func financeEvidenceNote(f *candFin) string {
	return strings.Join(earningsEvidenceFor(f).Notes, "；")
}
