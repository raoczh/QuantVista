package service

import (
	"math"
	"testing"
)

func TestIndicatorReviewFundPricePrecision(t *testing.T) {
	bars := chipFlatBars(210, 4.037, 2, 1000)
	for i := range bars {
		bars[i].High, bars[i].Low = 4.038, 4.036
	}
	snapshot := computeIndicatorSnapshot(4.037, bars)
	if snapshot == nil || snapshot.BollMid != 4.037 || math.Abs(snapshot.ATR14-.002) > 1e-9 {
		t.Fatalf("基金的布林价格和真实波幅不能因两位舍入失真：%+v", snapshot)
	}
	if snapshot.BollPos != 0 {
		t.Fatalf("平盘窗口没有布林带宽，不能因浮点尾差生成虚假的带内位置：%v", snapshot.BollPos)
	}
	row := computeWideRow("510300", wideStockMeta{Name: "基金样本"}, bars)
	if got := wideVal(row, "atr_14"); math.Abs(got-.002) > 1e-9 {
		t.Fatalf("筛选因子中的真实波幅也必须保留有效精度：%v", got)
	}
	if got := wideVal(row, "boll_pos"); !math.IsNaN(got) {
		t.Fatalf("平盘窗口的带内位置不可算，应保持缺失：%v", got)
	}
}

func TestChipReviewFundCostPrecision(t *testing.T) {
	bars := chipFlatBars(210, 4.037, 2, 1000)
	result, err := computeChipDistribution(bars, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.AvgCost-4.037) > .0002 || result.C90Low > 4.038 || result.C90High < 4.036 {
		t.Fatalf("4.037 元附近的筹码不能整体被舍入到 4.04 元：%+v", result.chipDay)
	}
}
