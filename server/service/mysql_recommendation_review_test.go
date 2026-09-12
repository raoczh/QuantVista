package service

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLRecommendationKeepsValidLongExplanation(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.Recommendation{})
	explanation := strings.Repeat("历史研究依据。", 4000)
	content, err := json.Marshal(map[string]any{"picks": []map[string]any{{"symbol": "600000", "action": "watch", "confidence": 60, "reason": []string{explanation}, "risks": []string{"本地风险说明"}, "evidence": []string{"price=10"}}}})
	if err != nil {
		t.Fatal(err)
	}
	picks, _, _, err := parseAndFilterPicks(string(content), map[string]candidate{"600000": {Symbol: "600000", Price: 10}}, 3)
	if err != nil || len(picks) != 1 {
		t.Fatalf("完整合法输出未被解析: %v", err)
	}
	detail, err := json.Marshal(picks[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(detail) <= 65535 {
		t.Fatal("夹具未超过 TEXT 容量")
	}
	row := model.Recommendation{UserID: 8830, BatchID: 1, Symbol: "600000", Market: "cn", Action: picks[0].Action, Confidence: int(picks[0].Confidence), DetailJSON: string(detail)}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("合法长说明导致推荐落库失败: %v", err)
	}
	var stored model.Recommendation
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DetailJSON != string(detail) {
		t.Error("长说明被截断或改写")
	}
}

func TestMySQLRecommendationGetKeepsOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.RecommendationBatch{}, &model.Recommendation{}, &model.RecommendationStatus{}, &model.Position{}, &model.PortfolioAccount{})
	batch, rec := seedLinkFixture(t, 8823, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	read, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	const callback = "review_recommendation_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_batches" && once.CompareAndSwap(false, true) {
			close(read)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
				tx.AddError(tx.Statement.Context.Err())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	type result struct {
		view *RecommendationView
		err  error
	}
	done := make(chan result, 1)
	go func() { v, e := (&RecommendationService{}).Get(8823, batch.ID); done <- result{v, e} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("详情未读取批次")
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&rec).Update("summary", "并发更新后的推荐摘要").Error; err != nil {
			return err
		}
		return tx.Create(&model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: 8823, Symbol: rec.Symbol, Market: "cn", Outcome: model.RecOutcomeStopLoss}).Error
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-done:
		if reply.err != nil || len(reply.view.Items) != 1 {
			t.Fatalf("读取详情: view=%+v err=%v", reply.view, reply.err)
		}
		if item := reply.view.Items[0]; item.Summary != "" || item.Status != nil {
			t.Errorf("批次读取后混入并发的新摘要或追踪状态: summary=%q status=%+v", item.Summary, item.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("详情未结束读取")
	}
}
