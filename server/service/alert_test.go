package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestEvaluateAlert_Price 到价：gte 用当日 high、lte 用当日 low 判盘中触达。
func TestEvaluateAlert_Price(t *testing.T) {
	// gte：现价未到但当日最高触及 → 命中。
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if !hit || msg == "" {
		t.Fatalf("当日最高触及应命中: hit=%v", hit)
	}
	// lte：当日最低触及止损位 → 命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpLTE, Threshold: 9},
		alertEval{Price: 9.5, DayHigh: 9.8, DayLow: 8.9},
	)
	if !hit {
		t.Fatalf("当日最低触及应命中")
	}
	// 未触及。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 12},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if hit {
		t.Fatalf("未触及不应命中")
	}
}

// TestEvaluateAlert_PctChange 异动：涨跌幅阈值。
func TestEvaluateAlert_PctChange(t *testing.T) {
	hit, v, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{ChangePct: 6.3},
	)
	if !hit || v != 6.3 {
		t.Fatalf("涨幅达标应命中: %v %v", hit, v)
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5},
		alertEval{ChangePct: -6.1},
	)
	if !hit {
		t.Fatalf("跌幅达标应命中")
	}
}

// TestEvaluateAlert_MA 均线：现价 vs MAn 站上/跌破，数据不足不命中。
func TestEvaluateAlert_MA(t *testing.T) {
	closes := []float64{9, 10, 11} // MA3 = 10
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.5, Closes: closes},
	)
	if !hit || msg == "" {
		t.Fatalf("站上 MA3 应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 9.5, Closes: closes},
	)
	if !hit {
		t.Fatalf("跌破 MA3 应命中")
	}
	// 数据不足：closes 少于 period。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 5},
		alertEval{Price: 10.5, Closes: closes},
	)
	if hit {
		t.Fatalf("日线不足不应命中")
	}
}

// TestEvaluateAlert_Breakout 突破：创近 N 日新高/新低（用当日 high/low）。
func TestEvaluateAlert_Breakout(t *testing.T) {
	highs := []float64{10.5, 10.8, 11.0} // 近 3 日前高 11.0
	lows := []float64{9.5, 9.2, 9.0}     // 近 3 日前低 9.0
	hit, _, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 11.3, DayHigh: 11.3, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新高应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 8.8, DayLow: 8.8, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新低应命中")
	}
	// 未破前高。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.9, DayHigh: 10.9, Highs: highs, Lows: lows},
	)
	if hit {
		t.Fatalf("未破前高不应命中")
	}
}

// TestAlertCRUDIsolation CRUD + 用户隔离 + 命中筛选（DB 集成）。
func TestAlertCRUDIsolation(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_rules")
	svc := &AlertService{}

	// 直接落库（跳过 Create 的行情校验，避免网络依赖）。
	r1 := &model.AlertRule{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11, Status: model.AlertStatusActive}
	r2 := &model.AlertRule{UserID: 1, Symbol: "000001", Market: "cn", Name: "平安银行",
		Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5, Status: model.AlertStatusTriggered}
	other := &model.AlertRule{UserID: 2, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 20, Status: model.AlertStatusActive}
	for _, r := range []*model.AlertRule{r1, r2, other} {
		if err := common.DB.Create(r).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	// List 用户1 → 2 条（隔离），triggered 排在前。
	rows, err := svc.List(1, "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("用户1 应有 2 条，得到 %d", len(rows))
	}
	if rows[0].Status != model.AlertStatusTriggered {
		t.Fatalf("命中的规则应排在前，得到 %s", rows[0].Status)
	}

	// TriggeredForUser → 仅 r2。
	trig, _ := svc.TriggeredForUser(1)
	if len(trig) != 1 || trig[0].ID != r2.ID {
		t.Fatalf("命中列表应只含 r2: %+v", trig)
	}

	// 跨用户 Update/Delete 隔离。
	if _, err := svc.Update(2, r1.ID, AlertInput{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 1}); err == nil {
		t.Fatalf("跨用户 Update 应失败")
	}
	if err := svc.Delete(2, r1.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// 暂停恢复：SetStatus。
	if _, err := svc.SetStatus(1, r2.ID, model.AlertStatusActive); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	var got model.AlertRule
	common.DB.First(&got, r2.ID)
	if got.Status != model.AlertStatusActive || got.TriggerMsg != "" {
		t.Fatalf("恢复后应 active 且清除命中标记: %+v", got)
	}

	// 本人删除。
	if err := svc.Delete(1, r1.ID); err != nil {
		t.Fatalf("本人删除失败: %v", err)
	}
	rows, _ = svc.List(1, "")
	if len(rows) != 1 {
		t.Fatalf("删除后应剩 1 条，得到 %d", len(rows))
	}
}
