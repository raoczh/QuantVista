package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// setupTestDB 用内存 SQLite 建库并迁移，供需要落库的测试使用。
func setupTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	if err := db.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	common.DB = db
}

// TestAnalysisHistoryAndGet 验证 History 的显式选列列名正确、Get 能取回详情、Delete 生效。
// 列名拼错会在此处的真实查询里暴露。
func TestAnalysisHistoryAndGet(t *testing.T) {
	setupTestDB(t)
	svc := &AnalysisService{}

	rec := &model.AnalysisRecord{
		UserID: 1, Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000",
		Target: "浦发银行", Title: "个股分析 · 浦发银行",
		Status: model.AnalysisStatusSuccess, Rating: model.AnalysisRatingBullish, Confidence: 66,
		Summary: "趋势向上", ResultJSON: `{"rating":"bullish","confidence":66,"summary":"趋势向上","disclaimer":"x"}`,
		DataSnapshot: `{"symbol":"600000"}`, Model: "gpt-x", Provider: "openai",
		PromptVersion: "p1", StrategyVersion: "s1", TotalTokens: 100,
	}
	if err := common.DB.Create(rec).Error; err != nil {
		t.Fatalf("插入记录失败: %v", err)
	}
	// 另一用户的记录，验证隔离。
	other := &model.AnalysisRecord{UserID: 2, Module: model.AnalysisModuleMarket, Status: model.AnalysisStatusSuccess, Summary: "别人的"}
	common.DB.Create(other)

	// History：只应看到 user 1 的记录，且不返回重字段。
	rows, err := svc.History(1, "all", 30)
	if err != nil {
		t.Fatalf("History 失败（列名可能拼错）: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 条（用户隔离），得到 %d", len(rows))
	}
	if rows[0].ResultJSON != "" || rows[0].DataSnapshot != "" {
		t.Fatalf("列表不应返回重字段: result=%q snap=%q", rows[0].ResultJSON, rows[0].DataSnapshot)
	}
	if rows[0].Summary != "趋势向上" {
		t.Fatalf("轻字段丢失: %+v", rows[0])
	}

	// Get：本人可取详情（含快照）。
	v, err := svc.Get(1, rec.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if v.Result == nil || v.Result.Rating != "bullish" {
		t.Fatalf("详情结构化结果解析失败: %+v", v)
	}
	if v.DataSnapshot == "" {
		t.Fatalf("详情应含数据快照供复现")
	}

	// 跨用户 Get 应视为不存在。
	if _, err := svc.Get(2, rec.ID); err == nil {
		t.Fatalf("跨用户 Get 应失败（隔离）")
	}
	// 跨用户 Delete 应视为不存在，且不误删。
	if err := svc.Delete(2, rec.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败（隔离）")
	}
	var cnt int64
	common.DB.Model(&model.AnalysisRecord{}).Where("id = ?", rec.ID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("越权删除后记录不应消失")
	}
	// 本人 Delete 生效。
	if err := svc.Delete(1, rec.ID); err != nil {
		t.Fatalf("本人 Delete 应成功: %v", err)
	}
}

// TestConsumeQuota 验证次数制配额记账：手动动作扣次、后台任务只记 token；熔断按次数判定。
func TestConsumeQuota(t *testing.T) {
	setupTestDB(t)

	q, err := getUserQuota(7)
	if err != nil {
		t.Fatalf("getUserQuota 失败: %v", err)
	}
	if q.ActionUsed != 0 || q.TokenUsed != 0 || q.RequestCount != 0 {
		t.Fatalf("新配额应为 0: %+v", q)
	}
	consumeQuota(7, 120, true) // 用户手动动作
	consumeQuota(7, 30, false) // 后台任务：只记 token 不扣次
	q2, _ := getUserQuota(7)
	if q2.TokenUsed != 150 || q2.RequestCount != 2 {
		t.Fatalf("token 审计累计错误: used=%d req=%d", q2.TokenUsed, q2.RequestCount)
	}
	if q2.ActionUsed != 1 {
		t.Fatalf("手动动作应只计 1 次，实际 %d", q2.ActionUsed)
	}

	// 熔断按次数：上限 1 且已用 1 → 拒绝；0 = 不限。
	if err := checkQuota(7); err != nil {
		t.Fatalf("未设上限不应熔断: %v", err)
	}
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", 7).Update("action_limit", 1)
	if err := checkQuota(7); err == nil {
		t.Fatalf("次数用尽应熔断")
	} else if RefusalCodeOf(err) != RefusalQuotaExhausted {
		t.Fatalf("次数用尽应挂机读码 %s, got %q / %v", RefusalQuotaExhausted, RefusalCodeOf(err), err)
	}
}

func TestCheckQuotaDatabaseFailureCode(t *testing.T) {
	oldDB := common.DB
	db, err := gorm.Open(sqlite.Open("file:quota_failure?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	common.DB = db
	t.Cleanup(func() { common.DB = oldDB })

	if got := RefusalCodeOf(checkQuota(99)); got != RefusalQuotaUnavailable {
		t.Fatalf("配额数据库读取失败不得误报 quota_exhausted，got %q", got)
	}
}
