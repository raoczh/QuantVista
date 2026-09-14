package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestFinancePresenceSurvivesStorageAndDoesNotRewardMissingGrowth(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	row := f10Row(t, "2025-12-31", "2025年报", 20, 0, 0)
	row["PARENTNETPROFITTZ"] = json.RawMessage(`null`)
	row["EPSJB"] = json.RawMessage(`"0"`)
	row["XSMLL"] = json.RawMessage(`"-"`)
	row["ZCFZL"] = json.RawMessage(`"NaN"`)
	old := fetchF10
	t.Cleanup(func() { fetchF10 = old })
	fetchF10 = func(context.Context, string) ([]datasource.DcRow, error) { return []datasource.DcRow{row}, nil }
	fetchFinanceIndicators(t.Context(), "600519")

	var stored model.FinanceIndicator
	if err := common.DB.Where("symbol = ?", "600519").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	if view["revenue_yoy"] != float64(0) || view["eps"] != float64(0) || view["net_profit_yoy"] != nil || view["gross_margin"] != nil || view["debt_ratio"] != nil {
		t.Fatalf("真实零与 null/非法字段必须在持久化和接口后仍可区分：%s", b)
	}
	budget := 0
	f := financeFactorFor(t.Context(), "600519", &budget)
	if f == nil || !f.has(f.RevenueYoY) || *f.RevenueYoY != 0 || f.NetProfitYoY != nil {
		t.Fatalf("候选财务可用性错误：%+v", f)
	}
	c := candidate{Fin: f}
	if score, _, status := qualityFinanceScore("leader", c); score != 0 || status != "partial" {
		t.Fatalf("未知净利同比不能当成未下降加分：%v %s", score, status)
	}
	if x := rankingCandidateFeatures(c); x[14] != 0 || !math.IsNaN(x[15]) {
		t.Fatalf("学习特征不能把真实零丢掉或给缺失补零：%v", x)
	}
	for _, value := range candidateLabeledValues(c) {
		if value.Path == "fin.net_profit_yoy" {
			t.Fatal("缺失净利同比不能进入可核验证据")
		}
	}
	brief := financeBrief(t.Context(), "600519")
	if brief == nil || brief["latest"].(map[string]any)["revenue_yoy"] != float64(0) || brief["latest"].(map[string]any)["net_profit_yoy"] != nil {
		t.Fatalf("AI 快照应与已知可用性一致：%v", brief)
	}
	snapshot := map[string]any{"finance": brief, "valuation": map[string]any{"pe": 0}}
	foundZero := false
	for _, value := range snapshotLabeledValues(snapshot, stockFieldHints(snapshot)) {
		if value.Path == "finance.latest.revenue_yoy" && value.Value == 0 {
			foundZero = true
		}
		if value.Path == "finance.latest.net_profit_yoy" || value.Path == "valuation.pe" {
			t.Fatal("未知字段不能被新财务零值语义放行")
		}
	}
	if !foundZero {
		t.Fatal("个股分析引用真实零值也应有快照证据")
	}

	// 上游补齐为真实 0，只有这时才允许表述为净利未下滑。
	row["PARENTNETPROFITTZ"] = json.RawMessage(`0`)
	fetchFinanceIndicators(t.Context(), "600519")
	f = financeFactorFor(t.Context(), "600519", &budget)
	if score, _, status := qualityFinanceScore("leader", candidate{Fin: f}); score != 7 || status != "available" {
		t.Fatalf("真实零必须可用：%v %s %+v", score, status, f)
	}
	// JSON 落盘/读取后 true zero 及年报出处不能丢失。
	b, err = json.Marshal(candidate{Fin: f})
	if err != nil {
		t.Fatal(err)
	}
	var again candidate
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	if x := rankingCandidateFeatures(again); x[13] != 20 || x[14] != 0 || x[15] != 0 {
		t.Fatalf("冻结候选重读后财务特征发生变化：%v", x)
	}
	// 后续响应再次缺失时，要连同掩码一起清掉，不能保留上一轮已知值。
	delete(row, "PARENTNETPROFITTZ")
	fetchFinanceIndicators(t.Context(), "600519")
	f = financeFactorFor(t.Context(), "600519", &budget)
	if f == nil || f.NetProfitYoY != nil {
		t.Fatalf("upsert 未清理已失效的可用性：%+v", f)
	}
}

func TestFinanceAnnualReferenceIsDisclosedAndFrozen(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	for _, row := range []model.FinanceIndicator{
		{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", NoticeDate: "2026-04-20", ROE: 5, NetProfitYoY: 12},
		{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", NoticeDate: "2026-05-02", ROE: 99},
		{Symbol: "600519", Market: "cn", ReportDate: "2024-12-31", NoticeDate: "2025-04-20", ROE: 20},
	} {
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	probe := inspectFinanceFactor("600519", "2026-05-01", time.Now())
	f := resolveFinanceFactor(probe, false)
	if f == nil || f.ROE == nil || *f.ROE != 5 || !f.hasAnnualROE() || *f.AnnualROE != 20 || f.AnnualReportDate != "2024-12-31" {
		t.Fatalf("不得把未披露年报或季报累计值用作年度质量：%+v", f)
	}
	if err := common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ? AND report_date = ?", "600519", "2025-12-31").Updates(map[string]any{"notice_date": "2026-04-25", "roe": 16}).Error; err != nil {
		t.Fatal(err)
	}
	if frozen := resolveFinanceFactor(probe, false); frozen == nil || *frozen.AnnualROE != 20 {
		t.Fatalf("未补拉候选不能被其他并发刷新改变冻结事实：%+v", frozen)
	}
	if refreshed := resolveFinanceFactor(probe, true); refreshed == nil || *refreshed.AnnualROE != 16 || refreshed.AnnualReportDate != "2025-12-31" {
		t.Fatalf("自身刷新后应选择最新已披露年报：%+v", refreshed)
	}
	// 已确认披露但缓存尚未补齐的年报，不能用旧年报填补。
	if err := common.DB.Where("symbol = ? AND report_date = ?", "600519", "2025-12-31").Delete(&model.FinanceIndicator{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.DisclosureSchedule{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ActualDate: "2026-04-25", IsPublished: true}).Error; err != nil {
		t.Fatal(err)
	}
	probe = inspectFinanceFactor("600519", "2026-05-01", time.Now())
	f = resolveFinanceFactor(probe, false)
	if !probe.RefreshNeeded || f == nil || f.AnnualROE != nil {
		t.Fatalf("已知年报缺口必须触发刷新且保留缺失：probe=%+v fin=%+v", probe, f)
	}
}

func TestFinanceScoreUsesAnnualROEAndBlocksIncompleteEvidence(t *testing.T) {
	c := candidate{PETTM: 12, PB: 1.2, Fin: &candFin{
		Version: financeFactorVersion, ReportDate: "2026-03-31", ROE: recNumber(4),
		AnnualReportDate: "2025-12-31", AnnualROE: recNumber(16), RevenueYoY: recNumber(15), NetProfitYoY: recNumber(0),
		NetProfit: recNumber(100), DeductProfit: recNumber(80),
	}}
	if score, _, status := qualityFinanceScore("leader", c); score != 7 || status != "available" {
		t.Fatalf("一季报低累计 ROE 不能覆盖已披露年度质量：%v %s", score, status)
	}
	c.Fin.ROE, c.Fin.AnnualROE = recNumber(40), recNumber(4)
	if score, _, _ := qualityFinanceScore("leader", c); score != 0 {
		t.Fatal("高季度 ROE 不能代替年度质量")
	}
	c.Fin.AnnualROE = nil
	q := &recSignalQuality{ATR: recNumber(1), MA20DistanceATR: recNumber(0), Stabilized: boolPtr(true)}
	if entry := entryQualityFor("value", "quality", c, q); entry.Status != "insufficient" {
		t.Fatalf("年度质量缺口不得放行可执行状态：%+v", entry)
	}
	c.Fin.AnnualROE, c.Fin.AnnualReportDate = recNumber(16), "2025-12-31"
	c.Fin.RevenueYoY = nil
	if entry := entryQualityFor("growth", "trend", c, q); entry.Status != "insufficient" {
		t.Fatalf("成长策略还必须有营收同比：%+v", entry)
	}
	c.Fin.Version = "" // 历史摘要无法证明零值的来源，仍保持未知。
	if c.Fin.has(c.Fin.NetProfitYoY) {
		t.Fatal("不得替历史摘要的零值补造可用性")
	}
}
