package service

import (
	"encoding/json"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestParseSnapshotMetaNew 新快照六字段直读。
func TestParseSnapshotMetaNew(t *testing.T) {
	snap := map[string]any{
		"captured_at":      "2026-07-17 15:30:00",
		"quote_as_of":      "2026-07-17 15:00:00",
		"bars_as_of":       "2026-07-17",
		"quote_source":     "eastmoney",
		"freshness_status": "fresh",
		"market_state":     "post_close",
	}
	b, _ := json.Marshal(snap)
	m := parseSnapshotMeta(string(b), time.Now())
	if m == nil || m.QuoteAsOf != "2026-07-17 15:00:00" || m.QuoteSource != "eastmoney" ||
		m.BarsAsOf != "2026-07-17" || m.FreshnessStatus != "fresh" || m.MarketState != "post_close" {
		t.Fatalf("六字段解析不符: %+v", m)
	}
}

// TestParseSnapshotMetaLegacy 旧快照仅 quote.data_time/source，兜底解析。
func TestParseSnapshotMetaLegacy(t *testing.T) {
	created := time.Date(2026, 7, 10, 9, 30, 0, 0, time.Local)
	snap := map[string]any{
		"quote": map[string]any{
			"data_time": "2026-07-10T14:55:00+08:00",
			"source":    "sina",
		},
		"recent_bars": []any{map[string]any{"d": "2026-07-10"}},
	}
	b, _ := json.Marshal(snap)
	m := parseSnapshotMeta(string(b), created)
	if m == nil {
		t.Fatalf("旧快照应兜底出 meta")
	}
	if m.QuoteAsOf != "2026-07-10 14:55:00" {
		t.Fatalf("quote_as_of 兜底不符: %q", m.QuoteAsOf)
	}
	if m.QuoteSource != "sina" {
		t.Fatalf("quote_source 兜底不符: %q", m.QuoteSource)
	}
	if m.BarsAsOf != "2026-07-10" {
		t.Fatalf("bars_as_of 兜底不符: %q", m.BarsAsOf)
	}
	if m.CapturedAt != "2026-07-10 09:30:00" {
		t.Fatalf("captured_at 应兜底为会话创建时间: %q", m.CapturedAt)
	}
}

// TestParseSnapshotMetaEmpty 空快照/无相关字段返回 nil。
func TestParseSnapshotMetaEmpty(t *testing.T) {
	if parseSnapshotMeta("", time.Time{}) != nil {
		t.Fatalf("空串应 nil")
	}
	if m := parseSnapshotMeta(`{"foo":1}`, time.Time{}); m != nil {
		t.Fatalf("无相关字段且无创建时间应 nil，got %+v", m)
	}
}

// TestQaGetSnapshotMeta Get 回传 SnapshotMeta 且清空 DataSnapshot。
func TestQaGetSnapshotMeta(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM ai_conversations")
	svc := &QaService{}
	snap := map[string]any{
		"quote":            map[string]any{"price": 10.0},
		"captured_at":      "2026-07-17 15:30:00",
		"quote_as_of":      "2026-07-17 15:00:00",
		"freshness_status": "fresh",
		"market_state":     "post_close",
		"quote_source":     "eastmoney",
	}
	b, _ := json.Marshal(snap)
	conv := &model.AiConversation{UserID: 5, Symbol: "600000", Market: "cn", Name: "浦发", DataSnapshot: string(b)}
	if err := common.DB.Create(conv).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	v, err := svc.Get(5, conv.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if v.SnapshotMeta == nil || v.SnapshotMeta.FreshnessStatus != "fresh" {
		t.Fatalf("SnapshotMeta 未回传: %+v", v.SnapshotMeta)
	}
	if v.DataSnapshot != "" {
		t.Fatalf("Get 应清空 DataSnapshot")
	}
}
