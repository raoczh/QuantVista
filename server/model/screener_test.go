package model

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type legacyScreenerStrategy struct {
	ID        int64  `gorm:"primaryKey"`
	UserID    int64  `gorm:"index"`
	Name      string `gorm:"size:64"`
	Desc      string `gorm:"size:256"`
	Period    string `gorm:"size:16"`
	Risk      string `gorm:"size:8"`
	TreeJSON  string `gorm:"type:text"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (legacyScreenerStrategy) TableName() string { return "screener_strategies" }

func openScreenerModelTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "screener.db")) + "?cache=private"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开 SQLite 测试库: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("获取 SQLite 连接: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestScreenerStrategyContentHashCanonicalAndComplete(t *testing.T) {
	treeA := `{
		"all": [
			{"factor":"close", "op":">", "value":1.0},
			{"factor":"turnover_rate", "op":"between", "value":-0, "value2":2e0}
		]
	}`
	treeB := `{"all":[{"value":1,"op":">","factor":"close"},{"value2":2.00,"value":0.0,"factor":"turnover_rate","op":"between"}]}`

	canonicalA, err := CanonicalScreenerTreeJSON(treeA)
	if err != nil {
		t.Fatalf("规范化 treeA: %v", err)
	}
	canonicalB, err := CanonicalScreenerTreeJSON(treeB)
	if err != nil {
		t.Fatalf("规范化 treeB: %v", err)
	}
	if canonicalA != canonicalB {
		t.Fatalf("等价 JSON 应规范化为相同文本:\nA=%s\nB=%s", canonicalA, canonicalB)
	}

	base, err := ScreenerStrategyContentHash("价值", "低估值", "mid", "low", treeA)
	if err != nil {
		t.Fatalf("计算基线 hash: %v", err)
	}
	same, err := ScreenerStrategyContentHash("价值", "低估值", "mid", "low", treeB)
	if err != nil {
		t.Fatalf("计算等价 hash: %v", err)
	}
	if len(base) != 64 || base != same {
		t.Fatalf("hash 应为稳定完整 SHA-256: base=%q same=%q", base, same)
	}

	variants := []struct {
		name, desc, period, risk, tree string
	}{
		{"价值2", "低估值", "mid", "low", treeA},
		{"价值", "另一描述", "mid", "low", treeA},
		{"价值", "低估值", "swing", "low", treeA},
		{"价值", "低估值", "mid", "high", treeA},
		{"价值", "低估值", "mid", "low", strings.Replace(treeA, `"value":1.0`, `"value":3`, 1)},
	}
	for i, variant := range variants {
		got, hashErr := ScreenerStrategyContentHash(
			variant.name, variant.desc, variant.period, variant.risk, variant.tree,
		)
		if hashErr != nil {
			t.Fatalf("variant %d 计算 hash: %v", i, hashErr)
		}
		if got == base {
			t.Errorf("完整快照字段变化未改变 hash，variant=%d", i)
		}
	}

	for _, raw := range []string{"", "[]", `{"all":[]} {"any":[]}`, "{"} {
		if _, err := CanonicalScreenerTreeJSON(raw); err == nil {
			t.Errorf("非法条件树 JSON 应被拒绝: %q", raw)
		}
	}
}

func TestMigrateScreenerStrategyRevisionsIdempotent(t *testing.T) {
	db := openScreenerModelTestDB(t)
	if err := db.AutoMigrate(&legacyScreenerStrategy{}); err != nil {
		t.Fatalf("创建 legacy 表: %v", err)
	}
	created := time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)
	legacyRows := []legacyScreenerStrategy{
		{UserID: 11, Name: "迁移策略甲", Desc: "甲", Period: "swing", Risk: "mid", TreeJSON: ` { "all" : [ { "value" : 1.0, "op" : ">", "factor" : "close" } ] } `, CreatedAt: created, UpdatedAt: created},
		{UserID: 12, Name: "迁移策略乙", Desc: "乙", Period: "mid", Risk: "low", TreeJSON: `{"any":[{"factor":"rsi_14","op":"<","value":30}]}`, CreatedAt: created.Add(time.Hour), UpdatedAt: created.Add(time.Hour)},
	}
	if err := db.Create(&legacyRows).Error; err != nil {
		t.Fatalf("写入 legacy 策略: %v", err)
	}
	if err := db.AutoMigrate(&ScreenerStrategy{}, &ScreenerStrategyRevision{}); err != nil {
		t.Fatalf("升级 schema: %v", err)
	}

	if err := migrateScreenerStrategyRevisions(db); err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	var first []ScreenerStrategyRevision
	if err := db.Order("strategy_id ASC").Find(&first).Error; err != nil {
		t.Fatalf("查询首次 revision: %v", err)
	}
	if len(first) != len(legacyRows) {
		t.Fatalf("首次迁移 revision 数=%d，want=%d", len(first), len(legacyRows))
	}
	for i, revision := range first {
		legacy := legacyRows[i]
		if revision.StrategyID != legacy.ID || revision.UserID != legacy.UserID || revision.Revision != 1 {
			t.Errorf("revision %d 归属/版本错误: %+v", i, revision)
		}
		wantTree, err := CanonicalScreenerTreeJSON(legacy.TreeJSON)
		if err != nil {
			t.Fatalf("计算期望 canonical tree: %v", err)
		}
		wantHash, err := ScreenerStrategyContentHash(legacy.Name, legacy.Desc, legacy.Period, legacy.Risk, legacy.TreeJSON)
		if err != nil {
			t.Fatalf("计算期望 hash: %v", err)
		}
		if revision.TreeJSON != wantTree || revision.ContentHash != wantHash {
			t.Errorf("revision %d 快照未规范化: tree=%s hash=%s", i, revision.TreeJSON, revision.ContentHash)
		}
		var strategy ScreenerStrategy
		if err := db.First(&strategy, legacy.ID).Error; err != nil {
			t.Fatalf("查询已迁移策略: %v", err)
		}
		if strategy.CurrentRevisionID != revision.ID {
			t.Errorf("strategy %d current_revision_id=%d，want=%d", strategy.ID, strategy.CurrentRevisionID, revision.ID)
		}
	}

	// 模拟进程在创建 revision 后、回填指针前退出；再次迁移必须复用原行。
	if err := db.Model(&ScreenerStrategy{}).Where("id = ?", legacyRows[1].ID).
		UpdateColumn("current_revision_id", 0).Error; err != nil {
		t.Fatalf("构造部分迁移态: %v", err)
	}
	if err := migrateScreenerStrategyRevisions(db); err != nil {
		t.Fatalf("部分态恢复迁移: %v", err)
	}
	if err := migrateScreenerStrategyRevisions(db); err != nil {
		t.Fatalf("第三次幂等迁移: %v", err)
	}
	var after []ScreenerStrategyRevision
	if err := db.Order("strategy_id ASC").Find(&after).Error; err != nil {
		t.Fatalf("查询重复迁移结果: %v", err)
	}
	if len(after) != len(first) {
		t.Fatalf("重复迁移产生了 revision: before=%d after=%d", len(first), len(after))
	}
	for i := range first {
		if after[i].ID != first[i].ID || after[i].ContentHash != first[i].ContentHash {
			t.Errorf("重复迁移改写了 revision: before=%+v after=%+v", first[i], after[i])
		}
	}
	var restored ScreenerStrategy
	if err := db.First(&restored, legacyRows[1].ID).Error; err != nil {
		t.Fatalf("查询恢复策略: %v", err)
	}
	if restored.CurrentRevisionID != first[1].ID {
		t.Errorf("部分迁移态未恢复指针: got=%d want=%d", restored.CurrentRevisionID, first[1].ID)
	}
}

func TestScreenerStrategyRevisionImmutableAndUnique(t *testing.T) {
	db := openScreenerModelTestDB(t)
	if err := db.AutoMigrate(&ScreenerStrategyRevision{}); err != nil {
		t.Fatalf("创建 revision 表: %v", err)
	}
	revision := ScreenerStrategyRevision{
		UserID: 7, StrategyID: 9, Revision: 1,
		ContentHash: strings.Repeat("a", 64), Name: "不可变", Period: "swing", Risk: "mid",
		TreeJSON: `{"all":[{"factor":"close","op":">","value":1}]}`,
	}
	if err := db.Create(&revision).Error; err != nil {
		t.Fatalf("创建 revision: %v", err)
	}
	if err := db.Model(&revision).Update("name", "被覆盖").Error; !errors.Is(err, ErrImmutableScreenerStrategyRevision) {
		t.Fatalf("更新历史 revision 应返回不可变错误，got=%v", err)
	}
	if err := db.Delete(&revision).Error; !errors.Is(err, ErrImmutableScreenerStrategyRevision) {
		t.Fatalf("删除历史 revision 应返回不可变错误，got=%v", err)
	}
	var stored ScreenerStrategyRevision
	if err := db.First(&stored, revision.ID).Error; err != nil {
		t.Fatalf("不可变 revision 被删除: %v", err)
	}
	if stored.Name != "不可变" {
		t.Errorf("不可变 revision 被更新: name=%q", stored.Name)
	}

	duplicate := revision
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("(strategy_id, revision) 重复行应被唯一索引拒绝")
	}
}
