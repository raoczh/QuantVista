package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestStoreCorporateActionsReplacesPendingPlan 同一期方案从预案推进到实施时，
// ex_date 会从空变成实际日期。空日期旧行必须被实施行替换，不能永久展示两份。
func TestStoreCorporateActionsReplacesPendingPlan(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	pending := model.CorporateAction{
		Symbol: "600000", Market: "cn", ReportDate: "2025-12-31",
		Progress: "董事会预案", DividendPretax: 1,
	}
	if err := common.DB.Create(&pending).Error; err != nil {
		t.Fatalf("建预案失败: %v", err)
	}
	implemented := model.CorporateAction{
		Symbol: "600000", Market: "cn", ReportDate: "2025-12-31",
		RecordDate: "2026-07-28", ExDate: "2026-07-29",
		Progress: model.CorpActionProgressImplemented, DividendPretax: 1,
	}
	if err := storeCorporateActions([]model.CorporateAction{implemented}); err != nil {
		t.Fatalf("存实施方案失败: %v", err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ? AND market = ? AND report_date = ?",
		"600000", "cn", "2025-12-31").Order("ex_date").Find(&rows)
	if len(rows) != 1 || rows[0].ExDate != "2026-07-29" {
		t.Fatalf("预案推进实施后应只保留实施行: %+v", rows)
	}

	// 模拟旧版本已同时留下预案与实施两行：下一轮同步须收敛空日期预案。
	legacyPending := implemented
	legacyPending.ID = 0
	legacyPending.ExDate, legacyPending.RecordDate = "", ""
	legacyPending.NoticeDate = "2026-03-01"
	if err := common.DB.Create(&legacyPending).Error; err != nil {
		t.Fatalf("建旧重复预案失败: %v", err)
	}
	implemented.PlanNoticeDate = "2026-03-01"
	if err := storeCorporateActions([]model.CorporateAction{implemented}); err != nil {
		t.Fatalf("收敛旧重复失败: %v", err)
	}
	rows = nil
	common.DB.Where("symbol = ? AND market = ? AND report_date = ?",
		"600000", "cn", "2025-12-31").Find(&rows)
	if len(rows) != 1 || rows[0].PlanNoticeDate != "2026-03-01" {
		t.Fatalf("旧预案与实施重复应收敛为实施行: %+v", rows)
	}

	// 同一报告期确有两次不同实施日时仍须并存，不能为了清预案误删真实分次实施。
	second := implemented
	second.ID = 0
	second.ExDate, second.RecordDate = "2026-09-01", "2026-08-31"
	second.PlanNoticeDate = "2026-08-01"
	second.PlanProfile = "特别分红"
	if err := storeCorporateActions([]model.CorporateAction{second}); err != nil {
		t.Fatalf("存分次实施方案失败: %v", err)
	}
	rows = nil
	common.DB.Where("symbol = ? AND market = ? AND report_date = ?",
		"600000", "cn", "2025-12-31").Order("ex_date").Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("不同实施日的两次方案应并存: %+v", rows)
	}
}

func TestStoreCorporateActionsDoesNotDeleteDifferentLegacyPlan(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	legacyOther := model.CorporateAction{
		Symbol: "600011", Market: "cn", ReportDate: "2025-12-31",
		NoticeDate: "2026-02-01", Progress: "董事会预案", DividendPretax: 1,
	}
	implemented := model.CorporateAction{
		Symbol: "600011", Market: "cn", ReportDate: "2025-12-31",
		NoticeDate: "2026-07-20", RecordDate: "2026-07-28", ExDate: "2026-07-29",
		Progress: model.CorpActionProgressImplemented, DividendPretax: 2,
	}
	if err := common.DB.Create(&[]model.CorporateAction{legacyOther, implemented}).Error; err != nil {
		t.Fatal(err)
	}
	implemented.ID = 0
	implemented.PlanNoticeDate = "2026-03-01"
	if err := storeCorporateActions([]model.CorporateAction{implemented}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", implemented.Symbol).Order("ex_date").Find(&rows)
	if len(rows) != 2 || rows[0].NoticeDate != legacyOther.NoticeDate ||
		rows[1].PlanNoticeDate != implemented.PlanNoticeDate {
		t.Fatalf("同步实施方案不得误删首次公告日不同的旧预案: %+v", rows)
	}
}

func TestStoreCorporateActionsMatchesLegacyNullPlanNoticeDate(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	legacy := model.CorporateAction{
		Symbol: "600012", Market: "cn", ReportDate: "2025-12-31",
		NoticeDate: "2026-03-01", Progress: "董事会预案", DividendPretax: 1,
	}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	// 复刻旧库 AutoMigrate 新增列后的真实形态：历史行是 SQL NULL，不是 Go 空串。
	if err := common.DB.Model(&model.CorporateAction{}).Where("id = ?", legacy.ID).
		UpdateColumn("plan_notice_date", nil).Error; err != nil {
		t.Fatal(err)
	}
	implemented := legacy
	implemented.ID = 0
	implemented.PlanNoticeDate = legacy.NoticeDate
	implemented.NoticeDate = "2026-07-20"
	implemented.RecordDate, implemented.ExDate = "2026-07-28", "2026-07-29"
	implemented.Progress = model.CorpActionProgressImplemented
	if err := storeCorporateActions([]model.CorporateAction{implemented}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", legacy.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].PlanNoticeDate != "2026-03-01" || rows[0].ExDate != "2026-07-29" {
		t.Fatalf("SQL NULL 的旧预案应接续为同一实施行: %+v", rows)
	}
}

func TestStoreCorporateActionsBlankIdentityIsIdempotent(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	plan := model.CorporateAction{
		Symbol: "000001", Market: "cn", ReportDate: "2026-06-30",
		NoticeDate: "2026-07-20", Progress: "董事会预案",
		DividendPretax: 2.46, PlanProfile: "10派2.46元(含税)",
	}
	if err := storeCorporateActions([]model.CorporateAction{plan}); err != nil {
		t.Fatal(err)
	}
	plan.DividendYield = 2.12
	if err := storeCorporateActions([]model.CorporateAction{plan}); err != nil {
		t.Fatalf("空 PLAN_NOTICE_DATE/ExDate 的真实预案重复同步必须幂等: %v", err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", plan.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].DividendYield != 2.12 {
		t.Fatalf("重复同步应更新同一预案行: %+v", rows)
	}
}

func TestStoreCorporateActionsKeepsDistinctPendingPlans(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	first := model.CorporateAction{
		Symbol: "600014", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-03-01",
		Progress: "董事会预案", DividendPretax: 1,
	}
	second := first
	second.PlanNoticeDate = "2026-08-01"
	second.NoticeDate = "2026-08-01"
	second.DividendPretax = 2
	if err := storeCorporateActions([]model.CorporateAction{first, second}); err != nil {
		t.Fatalf("存同报告期两份待实施预案失败: %v", err)
	}
	var rows []model.CorporateAction
	if err := common.DB.Where("symbol = ?", first.Symbol).
		Order("plan_notice_date ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].PlanNoticeDate != first.PlanNoticeDate ||
		rows[1].PlanNoticeDate != second.PlanNoticeDate {
		t.Fatalf("不同预案公告日且尚无除权日的两份方案必须并存: %+v", rows)
	}
}

func TestStoreCorporateActionsMissingPlanNoticeDoesNotEraseIdentity(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	existing := model.CorporateAction{
		Symbol: "600013", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29",
		Progress: model.CorpActionProgressImplemented, DividendPretax: 1,
	}
	if err := common.DB.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	incoming := existing
	incoming.ID = 0
	incoming.PlanNoticeDate = ""
	incoming.DividendYield = 1.23
	if err := storeCorporateActions([]model.CorporateAction{incoming}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", existing.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].PlanNoticeDate != existing.PlanNoticeDate || rows[0].DividendYield != 1.23 {
		t.Fatalf("上游漏稳定字段时应更新原行且保留已有身份: %+v", rows)
	}
}

func TestStoreCorporateActionsUpdatesCorrectedDateByPlanNotice(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	first := model.CorporateAction{
		Symbol: "600010", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	if err := storeCorporateActions([]model.CorporateAction{first}); err != nil {
		t.Fatal(err)
	}
	corrected := first
	corrected.ID = 0
	corrected.NoticeDate = "2026-07-25"
	corrected.RecordDate, corrected.ExDate = "2026-07-29", "2026-07-30"
	if err := storeCorporateActions([]model.CorporateAction{corrected}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", first.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].ExDate != "2026-07-30" || rows[0].NoticeDate != "2026-07-25" {
		t.Fatalf("同一预案的日期订正必须就地更新，不能留下旧日幽灵事件: %+v", rows)
	}
	second := corrected
	second.ID = 0
	second.PlanNoticeDate = "2026-08-10"
	// 两次独立方案可以碰巧在同一天实施；旧 (report_date, ex_date) 唯一索引会误拒绝。
	second.ExDate = corrected.ExDate
	if err := storeCorporateActions([]model.CorporateAction{second}); err != nil {
		t.Fatal(err)
	}
	common.DB.Where("symbol = ?", first.Symbol).Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("不同预案公告日代表分次实施，必须保留两行: %+v", rows)
	}
}

func TestStoreCorporateActionsDoesNotEraseStableIdentity(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	stable := model.CorporateAction{
		Symbol: "600013", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	if err := storeCorporateActions([]model.CorporateAction{stable}); err != nil {
		t.Fatal(err)
	}
	legacyPayload := stable
	legacyPayload.ID = 0
	legacyPayload.PlanNoticeDate = ""
	legacyPayload.NoticeDate = stable.NoticeDate
	if err := storeCorporateActions([]model.CorporateAction{legacyPayload}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", stable.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].PlanNoticeDate != stable.PlanNoticeDate ||
		rows[0].NoticeDate != legacyPayload.NoticeDate {
		t.Fatalf("缺稳定字段的更新不得擦除既有 PlanNoticeDate: %+v", rows)
	}
}

func TestStoreCorporateActionsMissingPlanAndChangedDatesFailsClosed(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	existing := model.CorporateAction{
		Symbol: "600015", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	if err := common.DB.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	incoming := existing
	incoming.ID = 0
	incoming.PlanNoticeDate = ""
	incoming.NoticeDate = "2026-07-25"
	incoming.RecordDate, incoming.ExDate = "2026-07-29", "2026-07-30"
	if err := storeCorporateActions([]model.CorporateAction{incoming}); err == nil {
		t.Fatal("缺稳定身份且 Ex/Notice 同时变化时无法区分订正与新方案，必须 fail closed")
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", existing.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].ID != existing.ID || rows[0].PlanNoticeDate != existing.PlanNoticeDate ||
		rows[0].ExDate != existing.ExDate {
		t.Fatalf("身份不明的载荷不得改写或新增方案: %+v", rows)
	}
}

func TestStoreCorporateActionsDoesNotMergeUnknownDifferentNotice(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	unknown := model.CorporateAction{
		Symbol: "600016", Market: "cn", ReportDate: "2025-12-31",
		NoticeDate: "2026-03-01", RecordDate: "2026-07-28", ExDate: "2026-07-29",
		DividendPretax: 1,
	}
	if err := common.DB.Create(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	independent := unknown
	independent.ID = 0
	independent.PlanNoticeDate = "2026-08-01"
	independent.NoticeDate = "2026-08-15"
	independent.DividendPretax = 2
	if err := storeCorporateActions([]model.CorporateAction{independent}); err == nil {
		t.Fatal("稳定 Plan 无法可靠关联同 Ex、不同 Notice 的旧弱身份行时必须 fail closed")
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", unknown.Symbol).Order("id ASC").Find(&rows)
	if len(rows) != 1 || rows[0].ID != unknown.ID || rows[0].PlanNoticeDate != "" ||
		rows[0].DividendPretax != unknown.DividendPretax {
		t.Fatalf("身份歧义不得覆盖旧行或创建可能重复的方案: %+v", rows)
	}
}

func TestStoreCorporateActionsMissingPlanRejectsAmbiguousCorrection(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	first := model.CorporateAction{
		Symbol: "600017", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	second := first
	second.PlanNoticeDate = "2026-04-01"
	second.NoticeDate = "2026-08-20"
	second.RecordDate, second.ExDate = "2026-08-28", "2026-08-29"
	if err := common.DB.Create(&[]model.CorporateAction{first, second}).Error; err != nil {
		t.Fatal(err)
	}
	incoming := first
	incoming.ID = 0
	incoming.PlanNoticeDate = ""
	incoming.NoticeDate = "2026-09-20"
	incoming.RecordDate, incoming.ExDate = "2026-09-28", "2026-09-29"
	if err := storeCorporateActions([]model.CorporateAction{incoming}); err == nil {
		t.Fatal("缺 PLAN_NOTICE_DATE 且改期后命中多个稳定候选时必须显式报歧义")
	}
	var count int64
	common.DB.Model(&model.CorporateAction{}).Where("symbol = ?", first.Symbol).Count(&count)
	if count != 2 {
		t.Fatalf("身份歧义不得创建可被下游消费的幽灵方案，得到 %d 行", count)
	}
}

func TestStoreCorporateActionsMissingPlanMatchesStablePending(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	existing := model.CorporateAction{
		Symbol: "600023", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-03-01",
		Progress: "董事会预案", DividendPretax: 1,
	}
	if err := common.DB.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	incoming := existing
	incoming.ID = 0
	incoming.PlanNoticeDate = ""
	incoming.DividendPretax = 2
	if err := storeCorporateActions([]model.CorporateAction{incoming}); err != nil {
		t.Fatal(err)
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", existing.Symbol).Find(&rows)
	if len(rows) != 1 || rows[0].ID != existing.ID ||
		rows[0].PlanNoticeDate != existing.PlanNoticeDate || rows[0].DividendPretax != 2 {
		t.Fatalf("稳定预案遇到缺 Plan/Ex 的重复载荷时应保持同一 ID: %+v", rows)
	}
}

func TestStoreCorporateActionsRejectsUnknownPlansWithDifferentNotice(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	first := model.CorporateAction{
		Symbol: "600024", Market: "cn", ReportDate: "2025-12-31",
		NoticeDate: "2026-03-01", Progress: "董事会预案", DividendPretax: 1,
	}
	second := first
	second.NoticeDate = "2026-08-01"
	second.DividendPretax = 2
	if err := storeCorporateActions([]model.CorporateAction{first, second}); err == nil {
		t.Fatal("缺 Plan/Ex 时公告日变化无法证明是独立预案，必须 fail closed")
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", first.Symbol).Order("notice_date ASC").Find(&rows)
	if len(rows) != 0 {
		t.Fatalf("同批事务中的身份歧义必须整体回滚，不得留下半批数据: %+v", rows)
	}
}

func TestStoreCorporateActionsMissingPlanDoesNotMergeSameExDifferentNotice(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	existing := model.CorporateAction{
		Symbol: "600025", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	if err := common.DB.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	independent := existing
	independent.ID = 0
	independent.PlanNoticeDate = ""
	independent.NoticeDate = "2026-08-20"
	independent.DividendPretax = 2
	if err := storeCorporateActions([]model.CorporateAction{independent}); err == nil {
		t.Fatal("缺 Plan、同 Ex 但 Notice 不同时无法判断订正或独立方案，必须 fail closed")
	}
	var rows []model.CorporateAction
	common.DB.Where("symbol = ?", existing.Symbol).Order("id ASC").Find(&rows)
	if len(rows) != 1 || rows[0].ID != existing.ID || rows[0].DividendPretax != 1 ||
		rows[0].NoticeDate != existing.NoticeDate {
		t.Fatalf("身份歧义不得改写旧方案或创建新方案: %+v", rows)
	}
}

func TestStoreCorporateActionsWeakMatchDoesNotRewriteReferencedAction(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	existing := model.CorporateAction{
		Symbol: "600026", Market: "cn", ReportDate: "2025-12-31",
		PlanNoticeDate: "2026-03-01", NoticeDate: "2026-07-20",
		RecordDate: "2026-07-28", ExDate: "2026-07-29", DividendPretax: 1,
	}
	if err := common.DB.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PositionCorpAdjust{
		UserID: 1, PositionID: 1, CorporateActionID: existing.ID,
		Symbol: existing.Symbol, Market: existing.Market, Status: model.CorpAdjustConfirmed,
	}).Error; err != nil {
		t.Fatal(err)
	}
	incoming := existing
	incoming.ID = 0
	incoming.PlanNoticeDate = ""
	incoming.NoticeDate = "2026-08-20"
	incoming.RecordDate, incoming.ExDate = "2026-08-28", "2026-08-29"
	incoming.DividendPretax = 9
	if err := storeCorporateActions([]model.CorporateAction{incoming}); err == nil {
		t.Fatal("弱身份载荷命中已被账本引用的 action 时必须报错，不能推进同步游标")
	}
	var after model.CorporateAction
	if err := common.DB.First(&after, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ExDate != existing.ExDate || after.NoticeDate != existing.NoticeDate || after.DividendPretax != 1 {
		t.Fatalf("弱身份载荷不得重新解释已被账本引用的 action: %+v", after)
	}
}

func TestStoreRestrictedReleasesReconcilesWindow(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	old := []model.RestrictedRelease{
		{Symbol: "600001", Market: "cn", FreeDate: "2026-08-01", FreeType: "定增", FreeShares: 100},
		{Symbol: "600002", Market: "cn", FreeDate: "2026-08-02", FreeType: "首发", FreeShares: 200},
		{Symbol: "600003", Market: "cn", FreeDate: "2026-10-01", FreeType: "首发", FreeShares: 300},
	}
	if err := common.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fresh := []model.RestrictedRelease{
		// 600001 改期：上游没有稳定批次 ID，窗口完整成功后用集合对账删除旧日期。
		{Symbol: "600001", Market: "cn", FreeDate: "2026-08-03", FreeType: "定增", FreeShares: 110},
		{Symbol: "600002", Market: "cn", FreeDate: "2026-08-02", FreeType: "首发", FreeShares: 220},
	}
	if err := storeRestrictedReleases(fresh, "2026-08-01", "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	var rows []model.RestrictedRelease
	common.DB.Order("symbol, free_date").Find(&rows)
	if len(rows) != 3 || rows[0].FreeDate != "2026-08-03" || rows[1].FreeShares != 220 ||
		rows[2].FreeDate != "2026-10-01" {
		t.Fatalf("窗口对账应删改期旧行、更新保留行且不碰窗口外: %+v", rows)
	}
}

func TestStoreIpoSubscriptionsUsesStableCodeAndReconcilesKind(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	old := []model.IpoSubscription{
		{Kind: model.IpoKindStock, Code: "301001", ApplyDate: "2026-08-01", ApplyCode: "301001"},
		{Kind: model.IpoKindStock, Code: "301001", ApplyDate: "2026-08-02", ApplyCode: "301001"},
		{Kind: model.IpoKindStock, Code: "301002", ApplyDate: "2026-08-03", ApplyCode: "301002"},
		{Kind: model.IpoKindCb, Code: "113001", ApplyDate: "2026-08-04", ApplyCode: "754001"},
	}
	if err := common.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fresh := []model.IpoSubscription{{
		Kind: model.IpoKindStock, Code: "301001", ApplyDate: "2026-08-05", ApplyCode: "301001",
		Name: "订正日期的新股",
	}}
	if err := storeIpoSubscriptions(model.IpoKindStock, fresh, "2026-08-01", "2026-09-30"); err != nil {
		t.Fatal(err)
	}
	var stocks []model.IpoSubscription
	common.DB.Where("kind = ?", model.IpoKindStock).Find(&stocks)
	if len(stocks) != 1 || stocks[0].Code != "301001" || stocks[0].ApplyDate != "2026-08-05" {
		t.Fatalf("同发行代码应就地改期、历史重复与取消项应清理: %+v", stocks)
	}
	var bonds int64
	common.DB.Model(&model.IpoSubscription{}).Where("kind = ?", model.IpoKindCb).Count(&bonds)
	if bonds != 1 {
		t.Fatalf("股票源对账不得清理转债源，剩余 %d", bonds)
	}
}
