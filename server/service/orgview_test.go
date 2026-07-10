package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanOrgView P3a 机构观点表清场 + 冷却表清空（包级共享状态，测试间互扰）。
func cleanOrgView(t *testing.T) {
	t.Helper()
	common.DB.Where("1 = 1").Delete(&model.ReportRating{})
	common.DB.Where("1 = 1").Delete(&model.OrgSurvey{})
	orgSyncMu.Lock()
	orgSyncTry = map[string]time.Time{}
	orgSyncMu.Unlock()
}

// orgNow 固定「今天」：纯函数手工验算的时间锚。
var orgNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.Local)

func orgDay(back int) string { return orgNow.AddDate(0, 0, -back).Format("2006-01-02") }

// computeOrgView：评级分布/变动检测/目标价中位与偏离/调研密度，全部手工验算。
func TestComputeOrgView(t *testing.T) {
	reports := []model.ReportRating{
		// 近 90 天：买入×2、增持×1；一次下调（最近）、一次首次覆盖；目标价 600/510。
		// 注意保持 report_date 降序——computeOrgView 声明降序输入（loadOrgRows 保证）。
		{ReportDate: orgDay(5), OrgName: "甲证券", Rating: "增持", LastRating: "买入", RatingChange: ratingChangeDown, TargetPrice: 510},
		// 无评级行：不进分布桶。
		{ReportDate: orgDay(10), OrgName: "己证券", Rating: "", RatingChange: -1},
		{ReportDate: orgDay(20), OrgName: "乙证券", Rating: "买入", RatingChange: ratingChangeKeep, TargetPrice: 600},
		{ReportDate: orgDay(40), OrgName: "丙证券", Rating: "买入", LastRating: "", RatingChange: ratingChangeFirst},
		// 90~180 天：中性×1（进 180 分布不进 90）；目标价 660。
		{ReportDate: orgDay(120), OrgName: "丁证券", Rating: "中性", RatingChange: ratingChangeKeep, TargetPrice: 660},
		// 180 天外：不进任何分布。
		{ReportDate: orgDay(200), OrgName: "戊证券", Rating: "买入", RatingChange: ratingChangeKeep, TargetPrice: 999},
	}
	surveys := []model.OrgSurvey{
		{SurveyDate: orgDay(3), OrgCount: 17},
		{SurveyDate: orgDay(45), OrgCount: 5},
		{SurveyDate: orgDay(80), OrgCount: 3},
		{SurveyDate: orgDay(120), OrgCount: 8}, // prev_90d 窗口
		{SurveyDate: orgDay(300), OrgCount: 2}, // 180 天外不计
	}

	ov := computeOrgView(reports, surveys, 500, orgNow)
	if ov == nil {
		t.Fatal("ov=nil")
	}

	d90 := ov["rating_dist_90d"].(map[string]any)
	if d90["buy"] != 2 || d90["overweight"] != 1 || d90["total"] != 3 {
		t.Errorf("90 天分布错: %v", d90)
	}
	d180 := ov["rating_dist_180d"].(map[string]any)
	if d180["buy"] != 2 || d180["overweight"] != 1 || d180["neutral"] != 1 || d180["total"] != 4 {
		t.Errorf("180 天分布错: %v", d180)
	}

	chg := ov["rating_changes_90d"].(map[string]any)
	if chg["upgrades"] != 0 || chg["downgrades"] != 1 || chg["first_covers"] != 1 {
		t.Errorf("变动计数错: %v", chg)
	}
	lc := ov["latest_rating_change"].(map[string]any)
	if lc["kind"] != "下调" || lc["from"] != "买入" || lc["to"] != "增持" || lc["org"] != "甲证券" {
		t.Errorf("最近变动错: %v", lc)
	}

	// 目标价（180 天窗）：510/600/660 → 中位 600，偏离 (600-500)/500=+20%。
	tp := ov["target_price"].(map[string]any)
	if tp["count"] != 3 || tp["min"] != 510.0 || tp["median"] != 600.0 || tp["max"] != 660.0 {
		t.Errorf("目标价统计错: %v", tp)
	}
	if tp["median_vs_price_pct"] != 20.0 {
		t.Errorf("目标价偏离错: %v", tp["median_vs_price_pct"])
	}

	sv := ov["survey"].(map[string]any)
	if sv["batches_30d"] != 1 || sv["batches_90d"] != 3 || sv["batches_prev_90d"] != 1 {
		t.Errorf("调研密度错: %v", sv)
	}
	if sv["latest_date"] != orgDay(3) || sv["latest_org_count"] != 17 {
		t.Errorf("最近调研错: %v", sv)
	}

	// 偶数个目标价中位数取均值：510/600 → 555。
	ov2 := computeOrgView([]model.ReportRating{reports[0], reports[2]}, nil, 0, orgNow)
	tp2 := ov2["target_price"].(map[string]any)
	if tp2["median"] != 555.0 {
		t.Errorf("偶数中位数应取均值: %v", tp2["median"])
	}
	if _, ok := tp2["median_vs_price_pct"]; ok {
		t.Error("price<=0 不应算偏离")
	}

	// 全空返回 nil。
	if computeOrgView(nil, nil, 100, orgNow) != nil {
		t.Error("全空应返回 nil")
	}
	// 只有调研没有研报：survey 段在、评级段缺席。
	ov3 := computeOrgView(nil, surveys, 100, orgNow)
	if ov3 == nil || ov3["survey"] == nil {
		t.Fatal("仅调研也应产出")
	}
	if _, ok := ov3["rating_dist_90d"]; ok {
		t.Error("无研报不应有评级分布")
	}
}

// aggregateSurveys：一机构一行明细按调研日聚合、取样封顶、降序。
func TestAggregateSurveys(t *testing.T) {
	rows := []datasource.SurveyRow{
		{SurveyDate: "2026-04-29", OrgName: "A1", OrgType: "证券公司", ReceiveWay: "业绩说明会", NoticeDate: "2026-04-29"},
		{SurveyDate: "2026-04-29", OrgName: "A2"},
		{SurveyDate: "2026-04-29", OrgName: "A3"},
		{SurveyDate: "2026-04-29", OrgName: "A4"},
		{SurveyDate: "2026-04-29", OrgName: "A5"},
		{SurveyDate: "2026-04-29", OrgName: "A6"}, // 超取样上限，只计数不进 names
		{SurveyDate: "2026-03-01", OrgName: "B1", ReceiveWay: "电话会议"},
		{SurveyDate: "", OrgName: "bad"}, // 缺日期丢弃
	}
	recs := aggregateSurveys("002230", rows)
	if len(recs) != 2 {
		t.Fatalf("应聚合成 2 行, got %d", len(recs))
	}
	if recs[0].SurveyDate != "2026-04-29" || recs[0].OrgCount != 6 {
		t.Errorf("聚合首行错: %+v", recs[0])
	}
	if recs[0].OrgNames != "A1,A2,A3,A4,A5" {
		t.Errorf("取样封顶错: %q", recs[0].OrgNames)
	}
	if recs[0].ReceiveWay != "业绩说明会" {
		t.Errorf("接待方式应取首行: %q", recs[0].ReceiveWay)
	}
	if recs[1].SurveyDate != "2026-03-01" || recs[1].OrgCount != 1 {
		t.Errorf("聚合次行错: %+v", recs[1])
	}
}

// ensure 双缓存：拉取落库 → 新鲜期内不回上游 → 失败也占冷却 → upsert 幂等。
func TestEnsureOrgViewCaching(t *testing.T) {
	setupTestDB(t)
	cleanOrgView(t)
	t.Cleanup(func() { cleanOrgView(t) })
	repCalls, svyCalls := 0, 0
	oldRep, oldSvy := fetchOrgReports, fetchOrgSurveys
	defer func() { fetchOrgReports, fetchOrgSurveys = oldRep, oldSvy }()
	fetchOrgReports = func(ctx context.Context, symbol string, days int) ([]datasource.ReportRow, error) {
		repCalls++
		return []datasource.ReportRow{
			{InfoCode: "AP1", Symbol: symbol, OrgName: "甲证券", PublishDate: orgDay(5), Rating: "买入", RatingChange: 3, TargetPrice: 512},
			{InfoCode: "AP2", Symbol: symbol, OrgName: "乙证券", PublishDate: orgDay(9), Rating: "增持", RatingChange: 2},
		}, nil
	}
	fetchOrgSurveys = func(ctx context.Context, symbol string, days int) ([]datasource.SurveyRow, error) {
		svyCalls++
		return []datasource.SurveyRow{
			{SurveyDate: orgDay(3), OrgName: "国盛证券", ReceiveWay: "业绩说明会"},
			{SurveyDate: orgDay(3), OrgName: "国元证券"},
		}, nil
	}

	ensureReportRatings(context.Background(), "600519")
	ensureOrgSurveys(context.Background(), "600519")
	if repCalls != 1 || svyCalls != 1 {
		t.Fatalf("首次应各拉一次: rep=%d svy=%d", repCalls, svyCalls)
	}
	var repCnt, svyCnt int64
	common.DB.Model(&model.ReportRating{}).Where("symbol = ?", "600519").Count(&repCnt)
	common.DB.Model(&model.OrgSurvey{}).Where("symbol = ?", "600519").Count(&svyCnt)
	if repCnt != 2 || svyCnt != 1 {
		t.Fatalf("落库行数错: rep=%d svy=%d（调研应聚合为 1 行）", repCnt, svyCnt)
	}
	var svy model.OrgSurvey
	common.DB.Where("symbol = ?", "600519").First(&svy)
	if svy.OrgCount != 2 || svy.OrgNames != "国盛证券,国元证券" {
		t.Fatalf("调研聚合错: %+v", svy)
	}

	// 新鲜期内不回上游。
	ensureReportRatings(context.Background(), "600519")
	ensureOrgSurveys(context.Background(), "600519")
	if repCalls != 1 || svyCalls != 1 {
		t.Fatalf("新鲜期内不应重拉: rep=%d svy=%d", repCalls, svyCalls)
	}

	// 非 A 股口径拒绝。
	ensureReportRatings(context.Background(), "AAPL")
	if repCalls != 1 {
		t.Fatal("非 6 位代码不应触发拉取")
	}

	// 过期 + 清冷却 → 重拉且 upsert 幂等。
	common.DB.Model(&model.ReportRating{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))
	common.DB.Model(&model.OrgSurvey{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))
	orgSyncMu.Lock()
	orgSyncTry = map[string]time.Time{}
	orgSyncMu.Unlock()
	ensureReportRatings(context.Background(), "600519")
	ensureOrgSurveys(context.Background(), "600519")
	if repCalls != 2 || svyCalls != 2 {
		t.Fatalf("过期后应重拉: rep=%d svy=%d", repCalls, svyCalls)
	}
	common.DB.Model(&model.ReportRating{}).Where("symbol = ?", "600519").Count(&repCnt)
	common.DB.Model(&model.OrgSurvey{}).Where("symbol = ?", "600519").Count(&svyCnt)
	if repCnt != 2 || svyCnt != 1 {
		t.Fatalf("upsert 应幂等: rep=%d svy=%d", repCnt, svyCnt)
	}

	// 失败也占冷却：清冷却前提下换失败实现，1h 内只打一次。
	fetchOrgReports = func(ctx context.Context, symbol string, days int) ([]datasource.ReportRow, error) {
		repCalls++
		return nil, datasource.ErrNoData
	}
	common.DB.Model(&model.ReportRating{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))
	orgSyncMu.Lock()
	orgSyncTry = map[string]time.Time{}
	orgSyncMu.Unlock()
	ensureReportRatings(context.Background(), "600519")
	ensureReportRatings(context.Background(), "600519")
	if repCalls != 3 {
		t.Fatalf("失败后 1h 冷却内不应重试: rep=%d", repCalls)
	}
}

// 值域同步铁律：org_view 段的数值叶子（目标价中位/偏离%）必须被 snapshotValueSet
// 自动收集——模型忠实引用机构目标价不得被误报幻觉。
func TestOrgViewValueSet(t *testing.T) {
	ov := computeOrgView([]model.ReportRating{
		{ReportDate: orgDay(5), OrgName: "甲证券", Rating: "买入", RatingChange: ratingChangeKeep, TargetPrice: 512.5},
	}, nil, 400, orgNow)
	vals := snapshotValueSet(map[string]any{"org_view": ov}, "recent_bars")
	for _, want := range []float64{512.5, 28.13} { // median 与 (512.5-400)/400×100 的 round2
		found := false
		for _, v := range vals {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("org_view 值域缺 %v（值域同步铁律）", want)
		}
	}
}

// orgViewBrief 与 Overview：有数据出段、无数据 nil/空结构、非 cn/基金拒绝。
func TestOrgViewBriefAndOverview(t *testing.T) {
	setupTestDB(t)
	cleanOrgView(t)
	t.Cleanup(func() { cleanOrgView(t) })
	oldRep, oldSvy := fetchOrgReports, fetchOrgSurveys
	defer func() { fetchOrgReports, fetchOrgSurveys = oldRep, oldSvy }()
	fetchOrgReports = func(ctx context.Context, symbol string, days int) ([]datasource.ReportRow, error) {
		return []datasource.ReportRow{
			{InfoCode: "AP1", Symbol: symbol, OrgName: "甲证券", PublishDate: orgDay(5), Rating: "买入", RatingChange: 3, TargetPrice: 512},
		}, nil
	}
	fetchOrgSurveys = func(ctx context.Context, symbol string, days int) ([]datasource.SurveyRow, error) {
		return nil, datasource.ErrNoData
	}

	brief := orgViewBrief(context.Background(), "600519", 400)
	if brief == nil {
		t.Fatal("brief=nil")
	}
	tp := brief["target_price"].(map[string]any)
	if tp["median"] != 512.0 || tp["median_vs_price_pct"] != 28.0 {
		t.Errorf("brief 目标价错: %v", tp)
	}
	if brief["survey"] != nil {
		t.Error("无调研不应有 survey 段")
	}
	if b := orgViewBrief(context.Background(), "AAPL", 100); b != nil {
		t.Error("非 A 股应返回 nil")
	}

	svc := NewOrgViewService()
	ov := svc.Overview(context.Background(), "cn", "600519", 400)
	if ov["summary"] == nil {
		t.Error("Overview summary 应存在")
	}
	if len(ov["reports"].([]model.ReportRating)) != 1 {
		t.Errorf("Overview reports 错: %v", ov["reports"])
	}
	// 非 cn 市场/基金代码返回空结构。
	if ov := svc.Overview(context.Background(), "us", "600519", 0); ov["summary"] != nil {
		t.Error("非 cn 市场应空")
	}
	if ov := svc.Overview(context.Background(), "cn", "510300", 0); ov["summary"] != nil {
		t.Error("ETF 应空")
	}
}
