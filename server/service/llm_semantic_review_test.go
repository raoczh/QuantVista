package service

import (
	"testing"

	"quantvista/model"
	"quantvista/setting"
)

func TestRecRiskRewardThresholdUsesUnroundedRatio(t *testing.T) {
	setupTestDB(t)
	old := setting.LLMSemanticValidator()
	if err := setting.SetLLMSemanticValidator(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setting.SetLLMSemanticValidator(old) })
	// 全部价格均为合法的两位小数；2.02/1.35=1.496296...，仍低于 1.5。
	pick := recPick{Action: model.RecActionBuy, BuyZoneLow: 10, BuyZoneHigh: 10, StopLoss: 8.65, TakeProfit: 12.02}
	if got := applyRecPickSemantics(pick); got.Action != model.RecActionWatch {
		t.Fatalf("舍入后的 1.50 不能通过 1.5 的最低赔率门槛：action=%s rr=%.8f", got.Action, recPickRRRatio(pick))
	}
	pick.StopLoss, pick.TakeProfit = 9, 11.5
	if got := applyRecPickSemantics(pick); got.Action != model.RecActionBuy {
		t.Fatal("恰好 1.5 的合法边界应保留")
	}
	pick.StopLoss, pick.TakeProfit = 9.7, 10.45
	if got := applyRecPickSemantics(pick); got.Action != model.RecActionBuy {
		t.Fatal("0.45/0.3 的浮点尾差不能使恰好 1.5 的合法边界被拒绝")
	}
}
