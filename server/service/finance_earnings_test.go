package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func earningsTestFactor() *candFin {
	return &candFin{Version: financeFactorVersion, ReportDate: "2026-06-30", NoticeDate: "2026-08-20",
		NetProfit: recNumber(130), DeductProfit: recNumber(110), NetProfitYoY: recNumber(30), RevenueYoY: recNumber(20),
		PriorNetProfit: recNumber(100), PriorReportDate: "2025-06-30", PriorNoticeDate: "2025-08-20",
		AnnualROE: recNumber(16), AnnualReportDate: "2025-12-31", OCFPS: recNumber(1)}
}

func TestEarningsGrowthNeedsPositiveComparableBaseAndCoreProfit(t *testing.T) {
	cases := []struct {
		name, state string
		change      func(*candFin)
		missing     bool
	}{
		{"亏损收窄", "loss_making", func(f *candFin) { f.NetProfit = recNumber(-20); f.PriorNetProfit = recNumber(-40) }, false},
		{"扭亏", "turnaround", func(f *candFin) { f.PriorNetProfit = recNumber(-40) }, false},
		{"零基数", "zero_base", func(f *candFin) { f.PriorNetProfit = recNumber(0) }, false},
		{"扣非亏损", "non_core_profit", func(f *candFin) { f.DeductProfit = recNumber(-1) }, false},
		{"真实零利润", "break_even", func(f *candFin) { f.NetProfit = recNumber(0) }, false},
		{"缺失利润", "unknown", func(f *candFin) { f.NetProfit = nil }, true},
		{"缺失扣非", "core_unknown", func(f *candFin) { f.DeductProfit = nil }, true},
		{"不同报告季", "base_unknown", func(f *candFin) { f.PriorReportDate = "2025-12-31" }, true},
		{"同比与同期金额冲突", "profitable", func(f *candFin) { f.NetProfit = recNumber(90) }, false},
	}
	positive, _, status := qualityFinanceScore("growth", candidate{Fin: earningsTestFactor(), PETTM: 15})
	if positive != 11 || status != "available" {
		t.Fatalf("完整正盈利基期的规则信用不应改变：%v %s", positive, status)
	}
	q := &recSignalQuality{ATR: recNumber(1), MA20DistanceATR: recNumber(0), Stabilized: boolPtr(true)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := earningsTestFactor()
			tc.change(f)
			if state := earningsEvidenceFor(f); state.Status != tc.state || f.hasPositiveBaseGrowth() {
				t.Fatalf("盈利性质错误：%+v", state)
			}
			c := candidate{Fin: f, PETTM: 15}
			score, notes, _ := qualityFinanceScore("growth", c)
			if score >= positive || strings.Contains(strings.Join(notes, " "), "营收与归母净利润双增长") {
				t.Fatalf("不能仅凭相同的正同比获得双增长加分：%v %v", score, notes)
			}
			entry := entryQualityFor("growth", "growth", c, q)
			if entry.Status == "aligned" || (tc.missing && entry.Status != "insufficient") {
				t.Fatalf("未验证的成长不得标为可执行：%+v", entry)
			}
			if technical := entryQualityFor("momentum", "trend", c, q); technical.Status != "aligned" {
				t.Fatalf("盈利质量纪律不能偷偷把技术策略改成财务筛选：%+v", technical)
			}
		})
	}
	f := earningsTestFactor()
	f.OCFPS = recNumber(-1)
	if score, _, _ := qualityFinanceScore("growth", candidate{Fin: f}); score != positive || len(financeEntryIssues("growth", f)) != 0 {
		t.Fatal("单期负现金流只提示核查，不无依据地增加扣分或阻断")
	}
}

func TestEarningsComparableReportIsDisclosedAndFrozen(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	for _, row := range []model.FinanceIndicator{
		{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", NoticeDate: "2026-08-20", NetProfit: 130, DeductProfit: 110, NetProfitYoY: 30},
		{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", NoticeDate: "2026-04-20", NetProfit: 999, ROE: 16},
		{Symbol: "600519", Market: "cn", ReportDate: "2025-06-30", NoticeDate: "2025-08-20", NetProfit: 100},
		{Symbol: "600519", Market: "cn", ReportDate: "2026-09-30", NoticeDate: "2026-10-20", NetProfit: 300},
	} {
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	probe := inspectFinanceFactor("600519", "2026-09-01", time.Now())
	f := resolveFinanceFactor(probe, false)
	if f == nil || !f.hasPositiveBaseGrowth() || f.PriorReportDate != "2025-06-30" || *f.PriorNetProfit != 100 {
		t.Fatalf("半年报必须对照已披露的上年半年报：%+v", f)
	}
	if err := common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ? AND report_date = ?", "600519", "2025-06-30").Update("net_profit", -100).Error; err != nil {
		t.Fatal(err)
	}
	if frozen := resolveFinanceFactor(probe, false); frozen == nil || frozen.Earnings.Status != "positive_base_growth" {
		t.Fatalf("并发更新不能改变已冻结的可比基期：%+v", frozen)
	}
	if refreshed := resolveFinanceFactor(probe, true); refreshed == nil || refreshed.Earnings.Status != "turnaround" {
		t.Fatalf("本轮确实刷新后应重新判定为扭亏：%+v", refreshed)
	}
	if comparableFinanceReportDate("2024-02-29") != "" || comparableFinanceReportDate("bad") != "" {
		t.Fatal("不存在的同期日期不能跨月拼接")
	}
}

func TestEarningsFactsKeepZeroAndAmountsAcrossJSON(t *testing.T) {
	f := earningsTestFactor()
	f.NetProfit, f.DeductProfit, f.PriorNetProfit = recNumber(-.01), recNumber(0), recNumber(-1)
	f.Earnings = earningsEvidenceFor(f)
	data, err := json.Marshal(candidate{Fin: f})
	if err != nil {
		t.Fatal(err)
	}
	var restored candidate
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Fin.Earnings.Status != "loss_making" || *restored.Fin.NetProfit != -.01 || !restored.Fin.has(restored.Fin.DeductProfit) {
		t.Fatalf("金额正负与已知零必须保留：%s", data)
	}
	found := false
	for _, v := range candidateLabeledValues(restored) {
		if v.Path == "fin.deduct_profit" && v.Value == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("AI 引用真实零扣非利润必须可核验")
	}
}

func TestEarningsSnapshotZeroAvailabilityAcrossVersions(t *testing.T) {
	for _, version := range []string{"ff1", financeFactorVersion} {
		snap := map[string]any{"finance": map[string]any{"version": version,
			"latest":           map[string]any{"net_profit": float64(0), "deduct_profit": nil},
			"comparable":       map[string]any{"net_profit": float64(0)},
			"statement_latest": map[string]any{"netcash_operate_yi": float64(0)},
		}}
		seen := map[string]bool{}
		for _, v := range snapshotLabeledValues(snap, stockFieldHints(snap)) {
			seen[v.Path] = true
		}
		if !seen["finance.latest.net_profit"] || seen["finance.latest.deduct_profit"] || seen["finance.statement_latest.netcash_operate_yi"] {
			t.Fatalf("新旧可空财务口径不能使缺失或旧三表零变成真实证据：%s %v", version, seen)
		}
		if version == financeFactorVersion && !seen["finance.comparable.net_profit"] {
			t.Fatal("新可比基期的真实零也必须可以核验")
		}
	}
}
