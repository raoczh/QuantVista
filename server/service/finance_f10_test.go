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
	common.DB.Where("1 = 1").Delete(&model.DisclosureSchedule{})
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

// SQLite 的 MAX(updated_at) 不保留时间列类型；finFresh 必须从真实列读取时间。
func TestFinFreshReadsSQLiteTimestamp(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
	})
	if !finFresh(&model.FinanceIndicator{}, "600519") {
		t.Fatal("刚写入的财务缓存应判为新鲜")
	}
	common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))
	if finFresh(&model.FinanceIndicator{}, "600519") {
		t.Fatal("超过 7 天的财务缓存应判为过期")
	}
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

// 仅 TTL 过期也属于 stale：没有完成一次有效刷新时，旧财务不得进入推荐。
func TestFinanceFactorForRejectsTTLStaleWithoutFreshRefresh(t *testing.T) {
	seedStale := func(t *testing.T) {
		t.Helper()
		if err := common.DB.Create(&model.FinanceIndicator{
			Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
			NoticeDate: time.Now().AddDate(0, 0, -30).Format("2006-01-02"), ROE: 34.2,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").
			Update("updated_at", time.Now().Add(-8*24*time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}

	t.Run("预算为零", func(t *testing.T) {
		setupTestDB(t)
		cleanF10(t)
		seedStale(t)
		oldF10 := fetchF10
		defer func() { fetchF10 = oldF10 }()
		calls := 0
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			calls++
			return nil, datasource.ErrNoData
		}

		budget := 0
		if fin := financeFactorFor(context.Background(), "600519", &budget); fin != nil {
			t.Fatalf("预算为零时 stale 财务必须缺失，fin=%+v", fin)
		}
		if calls != 0 || budget != 0 {
			t.Fatalf("预算为零不得请求上游：calls=%d budget=%d", calls, budget)
		}
	})

	t.Run("冷却命中", func(t *testing.T) {
		setupTestDB(t)
		cleanF10(t)
		seedStale(t)
		oldF10 := fetchF10
		defer func() { fetchF10 = oldF10 }()
		calls := 0
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			calls++
			return nil, datasource.ErrNoData
		}
		finSyncMu.Lock()
		finSyncTry["ind:600519"] = time.Now()
		finSyncMu.Unlock()

		budget := 1
		if fin := financeFactorFor(context.Background(), "600519", &budget); fin != nil {
			t.Fatalf("冷却命中时 stale 财务必须缺失，fin=%+v", fin)
		}
		if calls != 0 || budget != 1 {
			t.Fatalf("冷却命中不应消耗预算：calls=%d budget=%d", calls, budget)
		}
	})

	t.Run("请求失败", func(t *testing.T) {
		setupTestDB(t)
		cleanF10(t)
		seedStale(t)
		oldF10 := fetchF10
		defer func() { fetchF10 = oldF10 }()
		calls := 0
		fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
			calls++
			return nil, datasource.ErrNoData
		}

		budget := 1
		if fin := financeFactorFor(context.Background(), "600519", &budget); fin != nil {
			t.Fatalf("刷新失败时 stale 财务必须缺失，fin=%+v", fin)
		}
		if calls != 1 || budget != 0 {
			t.Fatalf("失败的真实请求仍应消耗预算：calls=%d budget=%d", calls, budget)
		}
	})
}

// 即使旧缓存刚写入，只要披露日历已确认有更新报告，推荐路径也必须尝试刷新。
func TestFinanceFactorForRefreshesPublishedNewReport(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	calls := 0
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		calls++
		return []datasource.DcRow{
			f10Row(t, "2026-03-31", "2026一季报", 10.57, 6.34, 1.47),
			f10Row(t, "2025-12-31", "2025年报", 34.2, 15.66, 15.38),
		}, nil
	}
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
		NoticeDate: time.Now().AddDate(0, 0, -30).Format("2006-01-02"), ROE: 34.2,
	})
	common.DB.Create(&model.DisclosureSchedule{
		Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", IsPublished: true,
		ActualDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
	})

	budget := 1
	fin := financeFactorFor(context.Background(), "600519", &budget)
	if calls != 1 || budget != 0 {
		t.Fatalf("新报告已披露时应绕过旧缓存刷新：calls=%d budget=%d", calls, budget)
	}
	if fin == nil || fin.Report != "2026一季报" {
		t.Fatalf("应返回刷新后的最新报告，fin=%+v", fin)
	}
}

// 已确认有新报告时，刷新失败必须 fail-closed，不能继续拿旧报告给长线策略加分。
func TestFinanceFactorForRejectsKnownStaleAfterRefreshFailure(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	calls := 0
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		calls++
		return nil, datasource.ErrNoData
	}
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
		NoticeDate: time.Now().AddDate(0, 0, -30).Format("2006-01-02"), ROE: 34.2,
	})
	common.DB.Create(&model.DisclosureSchedule{
		Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", IsPublished: true,
		ActualDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
	})

	budget := 1
	fin := financeFactorFor(context.Background(), "600519", &budget)
	if calls != 1 || budget != 0 {
		t.Fatalf("应尝试一次刷新：calls=%d budget=%d", calls, budget)
	}
	if fin != nil {
		t.Fatalf("已知过期的旧报告不得继续参与推荐，fin=%+v", fin)
	}
}

// 披露日位于未来的报告不是当前时点可用数据，既不能触发刷新，也不能直接读入推荐。
func TestFinanceFactorForExcludesFutureDisclosure(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	oldF10 := fetchF10
	defer func() { fetchF10 = oldF10 }()
	calls := 0
	fetchF10 = func(ctx context.Context, symbol string) ([]datasource.DcRow, error) {
		calls++
		return nil, datasource.ErrNoData
	}
	today := time.Now()
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
		NoticeDate: today.AddDate(0, 0, -30).Format("2006-01-02"), ROE: 34.2,
	})
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", ReportName: "2026一季报",
		NoticeDate: today.AddDate(0, 0, 1).Format("2006-01-02"), ROE: 99,
	})
	common.DB.Create(&model.DisclosureSchedule{
		Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", IsPublished: true,
		ActualDate: today.AddDate(0, 0, 1).Format("2006-01-02"),
	})
	common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").Update("updated_at", time.Now())
	if !finFresh(&model.FinanceIndicator{}, "600519") {
		t.Fatal("测试前置条件错误：缓存应处于 7 天容灾水位内")
	}

	budget := 1
	fin := financeFactorFor(context.Background(), "600519", &budget)
	if calls != 0 || budget != 1 {
		t.Fatalf("未来披露不得触发刷新：calls=%d budget=%d", calls, budget)
	}
	if fin == nil || fin.Report != "2025年报" || fin.ROE != 34.2 {
		t.Fatalf("未来 NoticeDate 行不得进入推荐，fin=%+v", fin)
	}
}

// 公告日缺失不能自动视为可用；只有披露日历能证明同报告期已发布时才允许进入推荐。
func TestFinanceFactorForRequiresProofForEmptyNoticeDate(t *testing.T) {
	setupTestDB(t)
	cleanF10(t)
	today := time.Now()
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "2025年报",
		NoticeDate: today.AddDate(0, 0, -30).Format("2006-01-02"), ROE: 34.2,
	})
	common.DB.Create(&model.FinanceIndicator{
		Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", ReportName: "未知公告日一季报",
		NoticeDate: "", ROE: 99,
	})
	common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").Update("updated_at", time.Now())

	budget := 0
	fin := financeFactorFor(context.Background(), "600519", &budget)
	if fin == nil || fin.Report != "2025年报" || fin.ROE != 34.2 {
		t.Fatalf("无披露证据的空公告日报表不得压过已知可用旧报告，fin=%+v", fin)
	}

	common.DB.Create(&model.DisclosureSchedule{
		Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", IsPublished: true,
		ActualDate: today.AddDate(0, 0, -1).Format("2006-01-02"),
	})
	fin = financeFactorFor(context.Background(), "600519", &budget)
	if fin == nil || fin.Report != "未知公告日一季报" || fin.ROE != 99 {
		t.Fatalf("披露日历已证明发布时应允许空公告日行，fin=%+v", fin)
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
