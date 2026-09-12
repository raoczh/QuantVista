package service

import (
	"encoding/json"
	"testing"
)

func TestEvidenceReviewKeepsQuotedPricePrecision(t *testing.T) {
	check := verifyEvidenceLabeled(secs("总结", "ETF 现价 4.037 元"), []labeledValue{{Path: "quote.price", Value: 4.037}})
	if len(check.Items) != 1 || !check.Items[0].Matched {
		t.Fatalf("合法报价应被核验命中：%+v", check)
	}
	if check.Items[0].Value != 4.037 || check.Items[0].SnapValue != 4.037 {
		t.Errorf("核验明细不能把实际引用和佐证价位改成两位：%+v", check.Items[0])
	}
	refs, _ := buildDebateEvidenceIndex(check)
	if len(refs) != 1 || refs[0].Value != 4.037 {
		t.Errorf("后续辩论收到的证据索引应与冻结报价一致：%+v", refs)
	}
}

func TestEvidenceReviewSameSnapshotProducesStableReferences(t *testing.T) {
	snapshot := map[string]any{
		"quote":      map[string]any{"price": 12.34, "prev_close": 12.35},
		"technicals": map[string]any{"ma5": 12.33, "ma10": 12.36, "ma20": 12.34},
	}
	var first string
	for i := 0; i < 100; i++ {
		values := snapshotLabeledValues(snapshot, nil)
		check := verifyEvidenceLabeled(secs("总结", "现价 12.34 元"), values)
		raw, err := json.Marshal(check)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(raw)
		} else if string(raw) != first {
			t.Fatalf("相同冻结快照和回答不能随机改变证据出处：\n%s\n%s", first, raw)
		}
	}
}

func TestEvidenceReviewHistoricalQuoteKeepsSourceDate(t *testing.T) {
	snapshot := map[string]any{
		"as_of":       "2026-09-06",
		"quote":       map[string]any{"trade_date": "2026-09-04", "source": "daily_bars", "price": 4.037},
		"technicals":  map[string]any{"ma5": 3.5, "bar_count": 60},
		"quant_score": map[string]any{"total": 55},
	}
	values := snapshotLabeledValues(snapshot, stockFieldHints(snapshot))
	for _, value := range values {
		if value.Path == "quote.price" || value.Path == "technicals.ma5" || value.Path == "quant_score.total" {
			if value.AsOf != "2026-09-04" || value.Source != "daily_bars" {
				t.Errorf("历史证据应保留真实交易日和来源：%+v", value)
			}
		}
	}
}
