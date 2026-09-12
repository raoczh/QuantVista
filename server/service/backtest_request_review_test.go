package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestBatchBacktestKeepsAShareScope(t *testing.T) {
	setupTestDB(t)
	axis := seedBacktestDB(t)
	svc := &BacktestService{benchFn: func(context.Context) []datasource.Bar { return fakeBench(axis) }}
	var batches []model.RecommendationBatch
	for _, market := range []string{"cn", "hk"} {
		batch := model.RecommendationBatch{UserID: 719, Type: model.RecTypeShortTerm, Market: market, Title: "市场回验", Status: model.RecStatusSuccess, CreatedAt: time.Now()}
		if err := common.DB.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
		symbol := "600001"
		if market == "hk" {
			symbol = "00700"
		}
		pick := model.Recommendation{BatchID: batch.ID, UserID: batch.UserID, Symbol: symbol, Market: market, Name: "审查样本", Action: model.RecActionBuy, RefPrice: 10}
		if err := common.DB.Create(&pick).Error; err != nil {
			t.Fatal(err)
		}
		batches = append(batches, batch)
	}
	if _, err := svc.BatchBacktest(t.Context(), 719, BatchBacktestRequest{BatchID: batches[1].ID}); err == nil || !strings.Contains(err.Error(), "A 股") {
		t.Errorf("港股批次应明确拒绝套用A股引擎，得到 %v", err)
	}
	all, err := svc.BatchBacktest(t.Context(), 719, BatchBacktestRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Batches != 1 || all.Picks != 1 || len(all.Rows) != 1 || all.Rows[0].Symbol != "600001" || !strings.Contains(strings.Join(all.Notes, ""), "非 A 股") {
		t.Errorf("全部回验仍把非A股批次算作A股样本或缺失行情: %+v", all)
	}
}

func TestBatchBacktestRejectsNegativeBatchID(t *testing.T) {
	setupTestDB(t)
	axis := seedBacktestDB(t)
	batch := model.RecommendationBatch{UserID: 720, Type: model.RecTypeShortTerm, Market: "cn", Title: "负数参数", Status: model.RecStatusSuccess, CreatedAt: time.Now()}
	if err := common.DB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	svc := &BacktestService{benchFn: func(context.Context) []datasource.Bar { return fakeBench(axis) }}
	if _, err := svc.BatchBacktest(t.Context(), batch.UserID, BatchBacktestRequest{BatchID: -1}); err == nil {
		t.Error("负数批次编号被当作全部批次执行")
	}
}
