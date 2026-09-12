package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestPositionPeakViewPreservesPricePrecision(t *testing.T) {
	p := model.Position{PeakPrice: 4.2375, PeakFrom: "2026-08-01", PeakDate: "2026-08-04"}
	view := peakViewFor(p, 4.0375, 4.0375, "2026-08-05")
	if view == nil || view.Price != 4.2375 {
		t.Errorf("峰值视图必须保留四位单价，不能在后端先截为两位：%+v", view)
	}
	if note := peakAdjustNote(4.0375, 2.0188); !strings.Contains(note, "4.0375") || !strings.Contains(note, "2.0188") {
		t.Errorf("折算流水的峰值说明必须保留实际单价：%s", note)
	}
}

func TestPositionPeakDetectsUnadjustedBarsWithMissingHigh(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 12062, "600901", "混合来源峰值", 10, 100, 10, "2026-08-01")
	bars := []model.DailyBar{
		{Market: "cn", Symbol: p.Symbol, TradeDate: "2026-08-04", Open: 10, Close: 10, Low: 10, High: 0, Source: "sina"},
		{Market: "cn", Symbol: p.Symbol, TradeDate: "2026-08-05", Open: 20, Close: 20, Low: 20, High: 20, Source: "eastmoney"},
	}
	if err := common.DB.Create(&bars).Error; err != nil {
		t.Fatal(err)
	}
	positions := []model.Position{*p}
	if _, err := syncPositionPeaksBefore(t.Context(), p.UserID, positions, "2026-08-06"); err != nil {
		t.Fatal(err)
	}
	if positions[0].PeakPrice != 10 {
		t.Fatalf("缺 high 的新浪历史仍是混合复权证据，不能过滤后把窗口标为可用于新峰值：%+v", positions[0])
	}
}
