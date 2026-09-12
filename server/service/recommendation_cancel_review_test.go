package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type cancelReviewRankingAdapter struct {
	recPreheatMarketAdapter
}

func (a *cancelReviewRankingAdapter) GetStockRanking(ctx context.Context, market, _ string, _ bool, _ int) ([]datasource.StockRank, error) {
	rows := make([]datasource.StockRank, 0, 3)
	for index := 0; index < 3; index++ {
		symbol := fmt.Sprintf("601%03d", index)
		quote, _ := a.GetQuote(ctx, market, symbol)
		rows = append(rows, datasource.StockRank{Symbol: symbol, Name: quote.Name, Price: quote.Price, Amount: 2e8, TurnoverRate: 3})
	}
	return rows, nil
}

func TestCancelBeforeRecommendationCommit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		once.Do(func() { close(entered) })
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	const userID int64 = 902
	seedRecEnv(t, userID, srv.URL, 0)
	previousRuntime := defaultJobRuntime
	runtime := newJobRuntime(1, 4)
	defaultJobRuntime = runtime
	t.Cleanup(func() {
		close(release)
		srv.Close()
		runtime.close()
		defaultJobRuntime = previousRuntime
	})
	adapter := &cancelReviewRankingAdapter{}
	market := NewMarketService(datasource.NewManagerWithAdapters(adapter))
	svc := NewRecommendationService(market, NewWatchlistService(market), NewLLMService())
	svc.em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
		return nil, 0, datasource.ErrNoData
	})
	for index := 0; index < 3; index++ {
		seedRecFreshFlow(t, fmt.Sprintf("601%03d", index), time.Now())
	}
	view, err := svc.Generate(context.Background(), userID, true, RecommendRequest{
		Type: model.RecTypeShortTerm, Market: "cn", Strategy: "momentum", Filters: &RecFilters{},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		var batch model.RecommendationBatch
		common.DB.First(&batch, view.ID)
		t.Fatalf("模型调用未开始，批次状态 %s，错误 %s", batch.Status, batch.Error)
	}
	var run model.JobRun
	if err := common.DB.Where("result_type = ? AND result_id = ?", JobResultRecommendation, view.ID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := common.DB.Model(&model.Recommendation{}).Where("batch_id = ?", view.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("取消前必须尚无结果，count=%d err=%v", count, err)
	}
	if _, err := CancelJobRun(userID, run.ID); err != nil {
		t.Fatal(err)
	}
	done := waitJobRun(t, userID, run.ID)
	if err := common.DB.Model(&model.Recommendation{}).Where("batch_id = ?", view.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if done.Status != model.JobStatusCanceled || count != 0 {
		t.Fatalf("结果提交前已取消：最终作业=%s，cancel_requested=%v，新增推荐条目=%d", done.Status, done.CancelRequested, count)
	}
}
