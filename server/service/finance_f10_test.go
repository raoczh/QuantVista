package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanF10 F2 财务表清场 + 冷却表清空（包级共享状态，测试间互扰）。
func cleanF10(t *testing.T) {
	t.Helper()
	common.DB.Where("1 = 1").Delete(&model.FinanceIndicator{})
	common.DB.Where("1 = 1").Delete(&model.FinanceStatement{})
	finSyncMu.Lock()
	finSyncTry = map[string]time.Time{}
	finSyncMu.Unlock()
}

func f10Row(t *testing.T, reportDate, name string, roe, revYoY, npYoY float64) datasource.DcRow {
	t.Helper()
	raw := `{"REPORT_DATE":"` + reportDate + ` 00:00:00","REPORT_DATE_NAME":"` + name + `","NOTICE_DATE":"2026-04-25 00:00:00",
		"EPSJB":1.5,"BPS":10.2,"MGJYXJJE":2.1,"TOTALOPERATEREVE":54702912385.23,"TOTALOPERATEREVETZ":` + jsonNum(revYoY) + `,
		"PARENTNETPROFIT":27242512886.45,"PARENTNETPROFITTZ":` + jsonNum(npYoY) + `,"KCFJCXSYJLR":100,"KCFJCXSYJLRTZ":1.2,
		"ROEJQ":` + jsonNum(roe) + `,"XSMLL":89.76,"XSJLL":52.22,"ZCFZL":12.12}`
	var m datasource.DcRow
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// ensureFinanceIndicators：拉取落库 → 新鲜期内不再回上游 → 冷却清空+过期后重拉。
func TestEnsureFinanceIndicators(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	calls := 0
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		calls++
		return []datasource.DcRow{
			f10Row(t, "2026-03-31", "2026一季报", 10.57, 6.34, 1.47),
			f10Row(t, "2025-12-31", "2025年报", 34.2, 15.66, 15.38),
		}, nil
	}

	ensureFinanceIndicators(context.Background(), "600519")
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	var rows []model.FinanceIndicator
	common.DB.Where("symbol = ?", "600519").Order("report_date DESC").Find(&rows)
	if len(rows) != 2 || rows[0].ReportName != "2026一季报" || rows[0].ROE != 10.57 || rows[0].RevenueYoY != 6.34 {
		t.Fatalf("落库错误: %+v", rows)
	}

	// 新鲜期内再调不回上游。
	ensureFinanceIndicators(context.Background(), "600519")
	if calls != 1 {
		t.Fatalf("新鲜期内不应重拉, calls=%d", calls)
	}

	// 非 A 股口径直接拒绝。
	ensureFinanceIndicators(context.Background(), "AAPL")
	if calls != 1 {
		t.Fatalf("非 6 位代码不应触发拉取")
	}

	// 缓存过期（手动把 updated_at 拨旧）+ 清冷却 → 重拉且 upsert 幂等（仍 2 行）。
	common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))
	finSyncMu.Lock()
	finSyncTry = map[string]time.Time{}
	finSyncMu.Unlock()
	ensureFinanceIndicators(context.Background(), "600519")
	if calls != 2 {
		t.Fatalf("过期后应重拉, calls=%d", calls)
	}
	var cnt int64
	common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").Count(&cnt)
	if cnt != 2 {
		t.Fatalf("upsert 应幂等, cnt=%d", cnt)
	}
}

// 拉取失败也占用尝试冷却（1h 内不重试），防打上游。
func TestEnsureFinanceCooldownOnFailure(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	calls := 0
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		calls++
		return nil, datasource.ErrNoData
	}
	ensureFinanceIndicators(context.Background(), "600519")
	ensureFinanceIndicators(context.Background(), "600519")
	if calls != 1 {
		t.Fatalf("失败后 1h 冷却内不应重试, calls=%d", calls)
	}
}

// financeBrief：latest/trend 结构与升序、无数据返回 nil。
func TestFinanceBrief(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		return []datasource.DcRow{
			f10Row(t, "2026-03-31", "2026一季报", 10.57, 6.34, 1.47),
			f10Row(t, "2025-12-31", "2025年报", 34.2, 15.66, 15.38),
		}, nil
	}
	brief := financeBrief(context.Background(), "600519")
	if brief == nil {
		t.Fatal("brief=nil")
	}
	latest := brief["latest"].(map[string]any)
	if latest["roe"] != 10.57 || latest["revenue_yi"] != 547.03 {
		t.Errorf("latest 错误: %v", latest)
	}
	trend := brief["trend"].([]map[string]any)
	if len(trend) != 2 || trend[0]["report"] != "2025年报" || trend[1]["report"] != "2026一季报" {
		t.Errorf("trend 应升序: %v", trend)
	}

	if b := financeBrief(context.Background(), "AAPL"); b != nil {
		t.Errorf("非 A 股应返回 nil")
	}
}

// financeFactorFor：缓存命中不耗预算；缺失耗预算拉取；预算耗尽返回 nil。
func TestFinanceFactorFor(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		return []datasource.DcRow{f10Row(t, "2025-12-31", "2025年报", 34.2, 15.66, 15.38)}, nil
	}

	budget := 1
	fin := financeFactorFor(context.Background(), "600519", &budget)
	if fin == nil || fin.ROE != 34.2 || fin.Report != "2025年报" {
		t.Fatalf("fin=%+v", fin)
	}
	if budget != 0 {
		t.Fatalf("应耗 1 预算, budget=%d", budget)
	}

	// 缓存命中：预算 0 也能读到。
	budget = 0
	if fin := financeFactorFor(context.Background(), "600519", &budget); fin == nil {
		t.Fatal("缓存命中不应依赖预算")
	}
	// 无缓存 + 预算耗尽 → nil 且不拉上游。
	if fin := financeFactorFor(context.Background(), "000001", &budget); fin != nil {
		t.Fatal("预算耗尽应返回 nil")
	}
}

// 长线策略财务加分：value ROE 档位、growth 双增速、业绩恶化扣分、缺失不动分。
func TestStrategyAdjustFinance(t *testing.T) {
	f := &candFactors{BarCount: 90, Pos60: 80} // Pos60>50 避免触发 value 的「未追高」加分干扰断言
	base := candidate{Symbol: "600519", Market: "cn", Price: 100}

	c := base
	c.Fin = &candFin{Report: "2025年报", ROE: 34.2, NetProfitYoY: 15.38, RevenueYoY: 15.66}
	delta, notes := strategyAdjust(model.RecTypeLongTerm, "value", c, f)
	if delta != 5+3 || !strings.Contains(strings.Join(notes, ";"), "ROE 34.2%") {
		t.Errorf("value 财务加分: delta=%v notes=%v", delta, notes)
	}

	// growth：营收 15.66（10~20 档 +3）+ 净利 15.38（15~30 档 +3）。
	if dg, _ := strategyAdjust(model.RecTypeLongTerm, "growth", c, f); dg != 6 {
		t.Errorf("growth 财务加分 delta=%v", dg)
	}

	c.Fin = &candFin{Report: "2025年报", ROE: 5, NetProfitYoY: -45, RevenueYoY: -10}
	dv, nv := strategyAdjust(model.RecTypeLongTerm, "value", c, f)
	if dv != -5 || !strings.Contains(strings.Join(nv, ";"), "业绩恶化") {
		t.Errorf("业绩恶化应扣 5: delta=%v notes=%v", dv, nv)
	}

	c.Fin = nil
	dn, _ := strategyAdjust(model.RecTypeLongTerm, "value", c, f)
	if dn != 0 {
		t.Errorf("财务缺失不动分: %v", dn)
	}
}

// F1 回归：业绩快报 upsert 冲突更新路径（GORM 把 YoY 转 yo_y，AssignmentColumns
// 必须用物理列名 revenue_yo_y/net_profit_yo_y——旧代码用 revenue_yoy 会 SQL 报错，
// 快报修正永远覆盖不进去）。
func TestUpsertExpressRowsIdempotent(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.EarningsExpress{})
	svc := NewFinanceService()
	row := func(revYoY float64) datasource.DcRow {
		raw := `{"SECURITY_CODE":"000001","SECURITY_NAME_ABBR":"平安银行","REPORT_DATE":"2026-06-30 00:00:00",
			"NOTICE_DATE":"2026-07-05 00:00:00","BASIC_EPS":1.1,"TOTAL_OPERATE_INCOME":100,"YSTZ":` + jsonNum(revYoY) + `,
			"PARENT_NETPROFIT":50,"JLRTBZCL":8.8,"WEIGHTAVG_ROE":11.5,"DATATYPE":"2026年 中报"}`
		var m datasource.DcRow
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err := svc.upsertExpressRows([]datasource.DcRow{row(5.5)}, "2026-06-30"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.upsertExpressRows([]datasource.DcRow{row(6.6)}, "2026-06-30"); err != nil {
		t.Fatalf("冲突更新失败: %v", err)
	}
	var rows []model.EarningsExpress
	common.DB.Where("symbol = ?", "000001").Find(&rows)
	if len(rows) != 1 || rows[0].RevenueYoY != 6.6 {
		t.Fatalf("应覆盖为 6.6: %+v", rows)
	}
}

// candidateLabeledValues 必须含 fin 数字（值域同步铁律）。
func TestCandidateValueSetFin(t *testing.T) {
	c := candidate{Price: 100, Fin: &candFin{ROE: 34.2, RevenueYoY: 15.66, NetProfitYoY: 15.38, GrossMargin: 91.9}}
	vals := candidateLabeledValues(c)
	for _, want := range []float64{34.2, 15.66, 15.38, 91.9} {
		if !labeledHas(vals, want) {
			t.Errorf("值域缺 %v", want)
		}
	}
}
