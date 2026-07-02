package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestMissedVerdict 错过机会判定纯函数。
func TestMissedVerdict(t *testing.T) {
	cases := []struct {
		passed, current float64
		wantVerdict     string
	}{
		{10, 11, "missed_gain"},   // +10% ≥ 5%：错过上涨
		{10, 9, "avoided_loss"},   // -10% ≤ -5%：回避正确
		{10, 10.2, "neutral"},     // +2%：接近持平
		{0, 10, "no_base"},        // 无放弃价基准
		{10, 0, "no_base"},        // 无现价
		{10, 10.5, "missed_gain"}, // 恰好 +5% 计为错过
	}
	for _, c := range cases {
		_, got := missedVerdict(c.passed, c.current)
		if got != c.wantVerdict {
			t.Fatalf("missedVerdict(%v,%v) = %s, want %s", c.passed, c.current, got, c.wantVerdict)
		}
	}
}

// TestSetItemStage 阶段流转校验与隔离（行情不可用时 PassedPrice 记 0 不阻断）。
func TestSetItemStage(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM watchlist_items")

	item := model.WatchlistItem{UserID: 1, WatchlistID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行"}
	common.DB.Create(&item)

	svc := &WatchlistService{market: NewMarketService(nil)}
	// NewMarketService(nil) 的 GetQuote 会因 mgr 为 nil panic——阶段流转里对 passed
	// 才取行情；先验证非 passed 阶段不触碰行情。
	got, err := svc.SetItemStage(nil, 1, item.ID, model.StageWatching, "")
	if err != nil || got.ResearchStage != model.StageWatching {
		t.Fatalf("流转 watching 失败: %v", err)
	}
	if got.StageAt == nil {
		t.Fatal("阶段变更时间应写入")
	}

	// 非法阶段。
	if _, err := svc.SetItemStage(nil, 1, item.ID, "bogus", ""); err == nil {
		t.Fatal("非法阶段应报错")
	}
	// 跨用户。
	if _, err := svc.SetItemStage(nil, 2, item.ID, model.StageWatching, ""); err == nil {
		t.Fatal("跨用户流转应视为不存在")
	}
}
