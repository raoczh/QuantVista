package service

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// buildSnapshotFixtureTable 手工两行宽表（其余因子列 NaN）。
func buildSnapshotFixtureTable() *FactorTable {
	t := &FactorTable{
		TradeDate: "2026-07-15", BuiltAt: time.Now(),
		Symbols:   []string{"600001", "600002"},
		Names:     []string{"甲股份", "乙股份"},
		LastDates: []string{"2026-07-15", "2026-07-10"}, // 乙停牌（stale）
		cols:      make(map[string][]float64, len(factorDefs)),
	}
	for _, d := range factorDefs {
		col := make([]float64, 2)
		col[0], col[1] = math.NaN(), math.NaN()
		t.cols[d.Key] = col
	}
	t.cols["close"][0] = 10.5
	t.cols["rsi_14"][0] = 56.2
	t.cols["above_ma20"][0] = 1
	t.cols["close"][1] = 8.8
	return t
}

// TestSnapshotFactorTable 落库内容 + 首写胜不可变 + 天数统计。
func TestSnapshotFactorTable(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM factor_snapshot_dailies")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM factor_snapshot_dailies") })

	ft := buildSnapshotFixtureTable()
	n, err := SnapshotFactorTable(ft)
	if err != nil || n != 2 {
		t.Fatalf("首次落库应 2 行: n=%d err=%v", n, err)
	}
	var row model.FactorSnapshotDaily
	if err := common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-15", "600001").First(&row).Error; err != nil {
		t.Fatalf("查快照失败: %v", err)
	}
	if row.LastBarDate != "2026-07-15" || row.FactorVersion != factorSnapshotVersion || row.Name != "甲股份" {
		t.Fatalf("元数据不符: %+v", row)
	}
	var vals map[string]float64
	if err := json.Unmarshal([]byte(row.FactorsJSON), &vals); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if vals["close"] != 10.5 || vals["rsi_14"] != 56.2 || vals["above_ma20"] != 1 {
		t.Fatalf("因子值不符: %v", vals)
	}
	if len(vals) != 3 {
		t.Fatalf("NaN 因子不应落键，应恰 3 键，得到 %d", len(vals))
	}
	// 乙股份：stale 也落库（LastBarDate 标记），仅 1 键。
	var row2 model.FactorSnapshotDaily
	if err := common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-15", "600002").First(&row2).Error; err != nil {
		t.Fatalf("stale 行也应落库: %v", err)
	}
	if row2.LastBarDate != "2026-07-10" {
		t.Fatalf("stale 标记不符: %+v", row2)
	}

	// 不可变：同日重跑（即便值已变）跳过不覆盖。
	ft.cols["close"][0] = 99.9
	n, err = SnapshotFactorTable(ft)
	if err != nil || n != 0 {
		t.Fatalf("同日重跑应跳过: n=%d err=%v", n, err)
	}
	var again model.FactorSnapshotDaily
	common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-15", "600001").First(&again)
	if again.FactorsJSON != row.FactorsJSON {
		t.Fatal("不可变纪律被破坏：快照被覆盖")
	}

	// 新交易日正常落库；天数统计=2。
	ft2 := buildSnapshotFixtureTable()
	ft2.TradeDate = "2026-07-16"
	if n, err := SnapshotFactorTable(ft2); err != nil || n != 2 {
		t.Fatalf("新交易日应落库: n=%d err=%v", n, err)
	}
	if d := FactorSnapshotDays(); d != 2 {
		t.Fatalf("快照天数应 2，得到 %d", d)
	}

	// 空表/nil 安全。
	if n, err := SnapshotFactorTable(nil); err != nil || n != 0 {
		t.Fatalf("nil 表应 0 行: n=%d err=%v", n, err)
	}
}
