package service

import (
	"strings"
	"testing"
)

// TestChangeOverN 近 N 日涨跌幅计算与边界。
func TestChangeOverN(t *testing.T) {
	closes := []float64{10, 10.5, 11, 10.8, 12} // 末值 12
	// 近 4 日：相对 closes[len-1-4]=closes[0]=10 → (12-10)/10*100 = 20。
	if got := changeOverN(closes, 4); got != 20 {
		t.Fatalf("近4日应 20%%，得到 %v", got)
	}
	// 近 2 日：相对 closes[2]=11 → (12-11)/11*100 ≈ 9.09。
	if got := changeOverN(closes, 2); got != 9.09 {
		t.Fatalf("近2日应 9.09%%，得到 %v", got)
	}
	// 数据不足：N >= len → 0。
	if got := changeOverN(closes, 5); got != 0 {
		t.Fatalf("数据不足应 0，得到 %v", got)
	}
	// prev 为 0 → 0（防除零）。
	if got := changeOverN([]float64{0, 5, 8}, 2); got != 0 {
		t.Fatalf("前值为0应 0，得到 %v", got)
	}
}

// TestAboveText / nameOr 辅助文案。
func TestCompareHelpers(t *testing.T) {
	if aboveText(true) != "站上MA20" || aboveText(false) != "位于MA20下方" {
		t.Fatalf("aboveText 文案错误")
	}
	if nameOr(CompareRow{Symbol: "600000"}) != "600000" {
		t.Fatalf("无名称应回退 symbol")
	}
	if nameOr(CompareRow{Symbol: "600000", Name: "浦发银行"}) != "浦发银行" {
		t.Fatalf("有名称应用名称")
	}
}

// TestCompareRowLine ETF 行不得携带估值段（腾讯源对基金 PE/PB 为 0，喂给模型是噪声），
// 且须显式标注基金身份；个股行有估值时正常拼入。
func TestCompareRowLine(t *testing.T) {
	fund := CompareRow{Symbol: "510300", Name: "沪深300ETF", QuoteOK: true, Price: 4.037, IsFund: true}
	line := compareRowLine(fund)
	if !strings.Contains(line, "ETF/场内基金") {
		t.Fatalf("基金行应带标注：%s", line)
	}
	if strings.Contains(line, "PE-TTM") {
		t.Fatalf("基金行不应携带估值段：%s", line)
	}
	stock := CompareRow{Symbol: "600000", Name: "浦发银行", QuoteOK: true, Price: 8.5, ValuationOK: true, PETTM: 5.2, PB: 0.4, TotalCap: 2500e8, TurnoverRate: 0.8}
	sline := compareRowLine(stock)
	if !strings.Contains(sline, "PE-TTM=5.20") || strings.Contains(sline, "ETF/场内基金") {
		t.Fatalf("个股行应带估值段且无基金标注：%s", sline)
	}
}
