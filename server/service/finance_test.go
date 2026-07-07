package service

import (
	"encoding/json"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// cleanFinanceTables setupTestDB 的内存库带 cache=shared（同进程测试间共享），
// F1 财报/披露/规则表的数据不按 user 隔离，涉及全局查询的测试先清场。
func cleanFinanceTables(t *testing.T) {
	t.Helper()
	for _, m := range []any{&model.DisclosureSchedule{}, &model.EarningsForecast{}, &model.EarningsExpress{},
		&model.Announcement{}, &model.AlertRule{}, &model.AlertEvent{}, &model.WatchlistItem{}, &model.Position{}} {
		common.DB.Where("1 = 1").Delete(m)
	}
}

// reportPeriodsAsOf：常规季、年报/一季报重叠季、跨年边界。
func TestReportPeriodsAsOf(t *testing.T) {
	cases := []struct {
		now  string
		want []string
	}{
		{"2026-07-07", []string{"2026-06-30", "2026-03-31"}},
		{"2026-04-15", []string{"2026-03-31", "2025-12-31"}}, // 年报+一季报并行披露季
		{"2026-01-05", []string{"2025-12-31", "2025-09-30"}},
		{"2026-10-01", []string{"2026-09-30", "2026-06-30"}},
		{"2026-03-31", []string{"2025-12-31", "2025-09-30"}}, // 季度末当天：该季尚未结束
	}
	for _, c := range cases {
		now, _ := time.ParseInLocation("2006-01-02", c.now, time.Local)
		got := reportPeriodsAsOf(now)
		if len(got) != 2 || got[0] != c.want[0] || got[1] != c.want[1] {
			t.Errorf("reportPeriodsAsOf(%s) = %v, want %v", c.now, got, c.want)
		}
	}
}

// upsert 幂等：同 (symbol, market, report_date) 重复写入应更新而非新增。
func TestUpsertFinanceRowsIdempotent(t *testing.T) {
	setupTestDB(t)
	svc := NewFinanceService()
	row := func(noticeDate, typ string) datasource.DcRow {
		raw := `{"SECURITY_CODE":"000001","SECURITY_NAME_ABBR":"平安银行","REPORT_DATE":"2026-06-30 00:00:00","NOTICE_DATE":"` + noticeDate + ` 00:00:00","PREDICT_TYPE":"` + typ + `","PREDICT_FINANCE":"净利润","ADD_AMP_LOWER":50.4,"ADD_AMP_UPPER":98.74,"PREDICT_CONTENT":"c","CHANGE_REASON_EXPLAIN":"r"}`
		var m datasource.DcRow
		_ = json.Unmarshal([]byte(raw), &m)
		return m
	}
	if _, err := svc.upsertForecastRows([]datasource.DcRow{row("2026-07-05", "预增")}, "2026-06-30"); err != nil {
		t.Fatal(err)
	}
	// 同键再写（预告修正）：应覆盖。
	if _, err := svc.upsertForecastRows([]datasource.DcRow{row("2026-07-07", "略增")}, "2026-06-30"); err != nil {
		t.Fatal(err)
	}
	var rows []model.EarningsForecast
	common.DB.Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("期望 1 行（幂等覆盖）, got %d", len(rows))
	}
	if rows[0].PredictType != "略增" || rows[0].NoticeDate != "2026-07-07" {
		t.Fatalf("应更新为最新值, got %+v", rows[0])
	}
	// 非 6 位代码（B 股/异常）应被过滤。
	bad := row("2026-07-07", "预增")
	bad["SECURITY_CODE"] = json.RawMessage(`"900901"`)
	ok := row("2026-07-07", "预增")
	ok["SECURITY_CODE"] = json.RawMessage(`"ABC123"`)
	if _, err := svc.upsertForecastRows([]datasource.DcRow{ok}, "2026-06-30"); err != nil {
		t.Fatal(err)
	}
	common.DB.Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("非法代码应被过滤, got %d 行", len(rows))
	}
}

// evaluateEarnDate：窗口命中、过期、防重。
func TestEvaluateEarnDate(t *testing.T) {
	now, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-07 10:00", time.Local)
	rule := model.AlertRule{Kind: model.AlertKindEarnDate, Threshold: 3}

	// 3 天后披露，阈值 3：命中。
	hit, days, msg := evaluateEarnDate(rule, "2026-07-10", "2026年 半年报", now)
	if !hit || days != 3 || msg == "" {
		t.Fatalf("3 天窗口应命中, got hit=%v days=%v msg=%q", hit, days, msg)
	}
	// 当天披露：命中且文案为「今日」。
	hit, _, msg = evaluateEarnDate(rule, "2026-07-07", "", now)
	if !hit || msg != "将于 07-07 披露 财报（今日）" {
		t.Fatalf("当天披露应命中, msg=%q", msg)
	}
	// 4 天后：未进窗口。
	if hit, _, _ = evaluateEarnDate(rule, "2026-07-11", "", now); hit {
		t.Fatal("超出阈值天数不应命中")
	}
	// 已过披露日：不命中。
	if hit, _, _ = evaluateEarnDate(rule, "2026-07-06", "", now); hit {
		t.Fatal("已过披露日不应命中")
	}
	// 防重：本窗口内已提醒过（TriggeredAt 晚于窗口起点）。
	trig := now.Add(-2 * time.Hour) // 今晨已提醒
	rule.TriggeredAt = &trig
	if hit, _, _ = evaluateEarnDate(rule, "2026-07-10", "", now); hit {
		t.Fatal("本窗口已提醒过不应重复命中")
	}
	// 上一窗口的旧提醒不影响新披露安排。
	old := now.AddDate(0, 0, -60)
	rule.TriggeredAt = &old
	if hit, _, _ = evaluateEarnDate(rule, "2026-07-10", "", now); !hit {
		t.Fatal("旧窗口提醒不应抑制新窗口命中")
	}
	// 无效日期。
	if hit, _, _ = evaluateEarnDate(rule, "", "", now); hit {
		t.Fatal("无预约日期不应命中")
	}
}

// evaluateEarnFcst：新预告命中、旧预告不命中、同份预告防重。
func TestEvaluateEarnFcst(t *testing.T) {
	now, _ := time.ParseInLocation("2006-01-02 15:04", "2026-07-07 19:30", time.Local)
	rule := model.AlertRule{Kind: model.AlertKindEarnFcst}
	fc := &model.EarningsForecast{
		NoticeDate: "2026-07-07", PredictType: "预增", PredictFinance: "净利润",
		AmpLower: 50.4, AmpUpper: 98.74,
	}
	hit, _, msg := evaluateEarnFcst(rule, fc, now)
	if !hit || msg == "" {
		t.Fatalf("当日新预告应命中, msg=%q", msg)
	}
	if want := "2026-07-07 发布业绩预告【预增】，净利润变动 50.40%~98.74%"; msg != want {
		t.Fatalf("msg=%q, want %q", msg, want)
	}
	// 超过新鲜窗口的旧预告。
	fc2 := &model.EarningsForecast{NoticeDate: "2026-06-01", PredictType: "预亏"}
	if hit, _, _ = evaluateEarnFcst(rule, fc2, now); hit {
		t.Fatal("超过新鲜窗口的旧预告不应命中")
	}
	// 防重：这份预告已提醒过（TriggeredAt 日期 >= NoticeDate）。
	trig := now
	rule.TriggeredAt = &trig
	if hit, _, _ = evaluateEarnFcst(rule, fc, now); hit {
		t.Fatal("同份预告不应重复命中")
	}
	// 更新的预告（新 NoticeDate 晚于上次提醒）应再次命中。
	older := now.AddDate(0, 0, -3)
	rule.TriggeredAt = &older
	if hit, _, _ = evaluateEarnFcst(rule, fc, now); !hit {
		t.Fatal("新发布的预告应再次命中")
	}
	// 无幅度时回退 Content 摘要。
	rule.TriggeredAt = nil
	fc3 := &model.EarningsForecast{NoticeDate: "2026-07-07", PredictType: "扭亏", Content: "预计扭亏为盈"}
	if _, _, msg = evaluateEarnFcst(rule, fc3, now); msg != "2026-07-07 发布业绩预告【扭亏】：预计扭亏为盈" {
		t.Fatalf("Content 回退文案错: %q", msg)
	}
	if hit, _, _ = evaluateEarnFcst(rule, nil, now); hit {
		t.Fatal("无预告不应命中")
	}
}

// 财报类提醒 DB 集成：命中落事件、规则行更新、once 置 triggered、同日不重复。
func TestEvaluateEarnRulesForUser(t *testing.T) {
	setupTestDB(t)
	cleanFinanceTables(t)
	svc := NewAlertService(nil)
	today := time.Now().In(time.Local)
	appoint := today.AddDate(0, 0, 2).Format("2006-01-02")

	common.DB.Create(&model.DisclosureSchedule{
		Symbol: "000001", Market: "cn", ReportDate: "2026-06-30", Name: "平安银行",
		AppointDate: appoint, ReportTypeName: "2026年 半年报",
	})
	common.DB.Create(&model.EarningsForecast{
		Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", Name: "贵州茅台",
		NoticeDate: today.Format("2006-01-02"), PredictType: "预增", PredictFinance: "净利润",
		AmpLower: 10, AmpUpper: 20,
	})
	rules := []model.AlertRule{
		{UserID: 7, Symbol: "000001", Market: "cn", Kind: model.AlertKindEarnDate,
			Op: model.AlertOpGTE, Threshold: 3, Once: true, Status: model.AlertStatusActive},
		{UserID: 7, Symbol: "600519", Market: "cn", Kind: model.AlertKindEarnFcst,
			Op: model.AlertOpGTE, Once: false, Status: model.AlertStatusActive},
		{UserID: 8, Symbol: "000001", Market: "cn", Kind: model.AlertKindEarnDate,
			Op: model.AlertOpGTE, Threshold: 1, Once: true, Status: model.AlertStatusActive}, // 窗口外（2 天后披露，阈值 1）
	}
	for i := range rules {
		common.DB.Create(&rules[i])
	}

	if hits := svc.evaluateEarnRulesForUser(7); hits != 2 {
		t.Fatalf("用户 7 应命中 2 条, got %d", hits)
	}
	if hits := svc.evaluateEarnRulesForUser(8); hits != 0 {
		t.Fatalf("用户 8 阈值外不应命中, got %d", hits)
	}
	var events []model.AlertEvent
	common.DB.Where("user_id = ?", 7).Find(&events)
	if len(events) != 2 {
		t.Fatalf("应落 2 条命中事件, got %d", len(events))
	}
	// once 规则命中后置 triggered。
	var r1 model.AlertRule
	common.DB.First(&r1, rules[0].ID)
	if r1.Status != model.AlertStatusTriggered || r1.TriggerMsg == "" {
		t.Fatalf("once 规则应置 triggered, got %+v", r1)
	}
	// 非 once 规则再评：earn_fcst 防重（同份预告），不应新增事件。
	if hits := svc.evaluateEarnRulesForUser(7); hits != 0 {
		t.Fatalf("同份预告再评不应命中, got %d", hits)
	}
	common.DB.Where("user_id = ?", 7).Find(&events)
	if len(events) != 2 {
		t.Fatalf("事件不应重复落, got %d", len(events))
	}
}

// EvaluateEarningsAll 遍历口径 + 盘中查询排除财报类。
func TestEarnKindSeparation(t *testing.T) {
	setupTestDB(t)
	cleanFinanceTables(t)
	common.DB.Create(&model.AlertRule{UserID: 9, Symbol: "300999", Market: "cn",
		Kind: model.AlertKindEarnDate, Op: model.AlertOpGTE, Threshold: 3, Status: model.AlertStatusActive})

	// 盘中口径的规则查询显式排除财报类：evaluateUserMarket 不应取到任何规则
	//（取到会去拉行情——空转）。这里 market service 为 nil，若未排除会 panic/报错。
	svc := NewAlertService(nil)
	if hits, err := svc.evaluateUserMarket(nil, 9); err != nil || hits != 0 {
		t.Fatalf("盘中口径应排除财报类规则, hits=%d err=%v", hits, err)
	}
	// 无披露数据时每日一评不命中也不报错。
	if hits := svc.EvaluateEarningsAll(); hits != 0 {
		t.Fatalf("无数据不应命中, got %d", hits)
	}
}

// validate：财报类 kind 的入参校验与归一。
func TestAlertValidateEarnKinds(t *testing.T) {
	svc := &AlertService{}
	in := AlertInput{Kind: "earn_date", Threshold: 5}
	if err := svc.validate(&in); err != nil {
		t.Fatalf("earn_date 合法入参不应报错: %v", err)
	}
	if in.Op != model.AlertOpGTE {
		t.Fatal("财报类 op 应归一为 gte")
	}
	in = AlertInput{Kind: "earn_date", Threshold: 0}
	if err := svc.validate(&in); err == nil {
		t.Fatal("earn_date 天数 0 应被拒")
	}
	in = AlertInput{Kind: "earn_fcst", Threshold: 99}
	if err := svc.validate(&in); err != nil {
		t.Fatalf("earn_fcst 不应校验 threshold: %v", err)
	}
	if in.Threshold != 0 {
		t.Fatal("earn_fcst threshold 应归零")
	}
	// kind 长度防列宽写库失败（AlertRule.Kind size:16）。
	for _, k := range earnAlertKinds {
		if len(k) > 16 {
			t.Errorf("kind %q 超过 16 字符（列宽 size:16）", k)
		}
	}
}

// announcementTitleTexts：原生结构与 JSON 反序列化结构两态。
func TestAnnouncementTitleTexts(t *testing.T) {
	snap := map[string]any{
		"announcements": map[string]any{
			"items": []announcementBrief{{Title: "关于回购股份的公告：回购价不超 12.50 元"}},
		},
	}
	got := announcementTitleTexts(snap)
	if len(got) != 1 || got[0] != "关于回购股份的公告：回购价不超 12.50 元" {
		t.Fatalf("原生结构提取失败: %v", got)
	}
	// 快照落库再读出（问答复用）是 []any/map[string]any 形态。
	b, _ := json.Marshal(snap)
	var snap2 map[string]any
	_ = json.Unmarshal(b, &snap2)
	got = announcementTitleTexts(snap2)
	if len(got) != 1 || got[0] != "关于回购股份的公告：回购价不超 12.50 元" {
		t.Fatalf("反序列化结构提取失败: %v", got)
	}
	if out := announcementTitleTexts(map[string]any{}); out != nil {
		t.Fatal("无公告块应返回 nil")
	}
}

// TomorrowDisclosures：只取自选∪持仓、只取次日、排除已披露。
func TestTomorrowDisclosures(t *testing.T) {
	setupTestDB(t)
	cleanFinanceTables(t)
	tomorrow := "2026-07-08"
	common.DB.Create(&model.WatchlistItem{UserID: 5, WatchlistID: 1, Symbol: "000001", Market: "cn"})
	common.DB.Create(&model.Position{UserID: 5, Symbol: "600519", Market: "cn"})
	rows := []model.DisclosureSchedule{
		{Symbol: "000001", Market: "cn", ReportDate: "2026-06-30", Name: "平安银行",
			AppointDate: tomorrow, ReportTypeName: "2026年 半年报"},
		{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", Name: "贵州茅台",
			AppointDate: "2026-07-09", ReportTypeName: "2026年 半年报"}, // 非次日
		{Symbol: "000002", Market: "cn", ReportDate: "2026-06-30", Name: "万科A",
			AppointDate: tomorrow, ReportTypeName: "2026年 半年报"}, // 非自选/持仓
		{Symbol: "000001", Market: "cn", ReportDate: "2026-03-31", Name: "平安银行",
			AppointDate: tomorrow, ReportTypeName: "2026年 一季报", IsPublished: true}, // 已披露
	}
	for i := range rows {
		common.DB.Create(&rows[i])
	}
	got := TomorrowDisclosures(5, tomorrow)
	if len(got) != 1 || got[0] != "平安银行(000001) 明日预约披露 2026年 半年报" {
		t.Fatalf("明日披露名单口径错: %v", got)
	}
	if out := TomorrowDisclosures(99, tomorrow); len(out) != 0 {
		t.Fatalf("无自选持仓用户应为空: %v", out)
	}
}
