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
	common.DB.Where("1 = 1").Delete(&model.MarketSyncState{})
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM factor_snapshot_dailies")
		common.DB.Where("1 = 1").Delete(&model.MarketSyncState{})
	})
	// 快照只落 init_status=done 的 symbol：先把 fixture 三只标记为 done。
	for _, sym := range []string{"600001", "600002", "600003"} {
		common.DB.Create(&model.MarketSyncState{Symbol: sym, Market: "cn", InitStatus: "done"})
	}

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

	// 同日补缺：分批初始化场景——新表比已有快照多出 600003，只补缺失行、
	// 已有行仍不可覆盖（旧的「首写胜整日跳过」会让补齐股票永久缺席）。
	ft3 := buildSnapshotFixtureTable()
	ft3.Symbols = append(ft3.Symbols, "600003")
	ft3.Names = append(ft3.Names, "丙股份")
	ft3.LastDates = append(ft3.LastDates, "2026-07-15")
	for _, d := range factorDefs {
		ft3.cols[d.Key] = append(ft3.cols[d.Key], math.NaN())
	}
	ft3.cols["close"][0] = 77.7 // 已有行的新值：不得覆盖
	ft3.cols["close"][2] = 6.6
	if n, err := SnapshotFactorTable(ft3); err != nil || n != 1 {
		t.Fatalf("同日应只补缺失的 1 行: n=%d err=%v", n, err)
	}
	var added model.FactorSnapshotDaily
	if err := common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-15", "600003").First(&added).Error; err != nil {
		t.Fatalf("补缺行应落库: %v", err)
	}
	common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-15", "600001").First(&again)
	if again.FactorsJSON != row.FactorsJSON {
		t.Fatal("补缺时已有行被覆盖")
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

// TestSnapshotInitDoneFilter 落库门槛：只固化 init_status=done 的 symbol；pending 股
// 不落（避免首部署当天短史低质量因子被首写胜永久冻结），其 init 完成后由后续 rebuild
// 的「补缺失 symbol」逻辑自然补上。
func TestSnapshotInitDoneFilter(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM factor_snapshot_dailies")
	common.DB.Where("1 = 1").Delete(&model.MarketSyncState{})
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM factor_snapshot_dailies")
		common.DB.Where("1 = 1").Delete(&model.MarketSyncState{})
	})

	// 600501 已初始化完成、600502 仍 pending（历史未补齐）。
	common.DB.Create(&model.MarketSyncState{Symbol: "600501", Market: "cn", InitStatus: "done"})
	common.DB.Create(&model.MarketSyncState{Symbol: "600502", Market: "cn", InitStatus: "pending"})

	ft := &FactorTable{
		TradeDate: "2026-07-20", BuiltAt: time.Now(),
		Symbols:   []string{"600501", "600502"},
		Names:     []string{"甲", "乙"},
		LastDates: []string{"2026-07-20", "2026-07-20"},
		cols:      make(map[string][]float64, len(factorDefs)),
	}
	for _, d := range factorDefs {
		ft.cols[d.Key] = []float64{math.NaN(), math.NaN()}
	}
	ft.cols["close"][0] = 12.3
	ft.cols["close"][1] = 4.5

	// 首轮：只落 done 的 600501，pending 的 600502 缺席。
	if n, err := SnapshotFactorTable(ft); err != nil || n != 1 {
		t.Fatalf("首轮应只落 done 的 1 行: n=%d err=%v", n, err)
	}
	var pendingN int64
	common.DB.Model(&model.FactorSnapshotDaily{}).
		Where("trade_date = ? AND symbol = ?", "2026-07-20", "600502").Count(&pendingN)
	if pendingN != 0 {
		t.Fatalf("pending 股不应落快照, got %d", pendingN)
	}

	// 600502 历史初始化完成后再 rebuild：同日「补缺失 symbol」把它补上，
	// 已有的 600501 仍不可覆盖。
	common.DB.Model(&model.MarketSyncState{}).
		Where("symbol = ?", "600502").Update("init_status", "done")
	ft.cols["close"][0] = 99.9 // 已有行的新值：不得覆盖
	if n, err := SnapshotFactorTable(ft); err != nil || n != 1 {
		t.Fatalf("done 后应补上缺失的 1 行: n=%d err=%v", n, err)
	}
	var added model.FactorSnapshotDaily
	if err := common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-20", "600502").First(&added).Error; err != nil {
		t.Fatalf("done 后补缺行应落库: %v", err)
	}
	var kept model.FactorSnapshotDaily
	common.DB.Where("trade_date = ? AND symbol = ?", "2026-07-20", "600501").First(&kept)
	var vals map[string]float64
	json.Unmarshal([]byte(kept.FactorsJSON), &vals)
	if vals["close"] != 12.3 {
		t.Fatalf("补缺时已有行被覆盖: close=%v", vals["close"])
	}
}
