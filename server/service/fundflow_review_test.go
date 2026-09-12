package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func setupFlowReview(t *testing.T) *datasource.EastMoneyAdapter {
	t.Helper()
	setupTestDB(t)
	resetRecommendationPreheatState(t)
	t.Cleanup(func() { resetRecommendationPreheatState(t) })
	return datasource.NewEastMoneyAdapter()
}

func flowReviewBody(date string) []byte {
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"klines": []string{date + ",10000000,0,0,0,0,2,0,0,0,0,10.00,0.10,0,0"}}})
	return body
}

func TestFundFlowReviewAfterCloseFetchesTodayWithYesterdayCached(t *testing.T) {
	em := setupFlowReview(t)
	if err := common.DB.Create(&model.FundFlowDaily{Market: "cn", Symbol: "600519", TradeDate: "2026-09-08", MainNet: 1}).Error; err != nil {
		t.Fatal(err)
	}
	var calls int
	em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
		calls++
		return flowReviewBody("2026-09-09"), 200, nil
	})
	now := time.Date(2026, 9, 9, 16, 30, 0, 0, time.Local)
	flows, fresh := ensureStockFundFlowAt(t.Context(), em, "cn", "600519", nil, now)
	if calls != 1 || !fresh || len(flows) != 2 || flows[len(flows)-1].TradeDate != "2026-09-09" {
		t.Fatalf("盘后已有昨日缓存也应补今日数据：calls=%d fresh=%v flows=%+v", calls, fresh, flows)
	}
}

func TestFundFlowReviewFailuresAreNotEmptyData(t *testing.T) {
	for _, stage := range []string{"upstream", "read", "write"} {
		t.Run(stage, func(t *testing.T) {
			em := setupFlowReview(t)
			injected := errors.New("模拟资金流" + stage + "故障")
			em.SetFetchForTest(func(context.Context, string, map[string]string) ([]byte, int, error) {
				if stage == "upstream" {
					return nil, 0, injected
				}
				return flowReviewBody(prevOpenTradeDate(time.Now().Format("2006-01-02"))), 200, nil
			})
			const hook = "review_fundflow_database_failure"
			fail := func(tx *gorm.DB) {
				if tx.Statement.Table == "fund_flow_dailies" {
					tx.AddError(injected)
				}
			}
			if stage == "read" {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, fail); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			}
			if stage == "write" {
				if err := common.DB.Callback().Create().Before("gorm:create").Register(hook, fail); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Create().Remove(hook) })
			}
			svc := &MoodService{em: em}
			for attempt := 0; attempt < 2; attempt++ {
				if view, err := svc.StockFundFlow(t.Context(), "cn", "600519", 90); err == nil || view != nil {
					t.Errorf("首次失败及冷却内再读都不能声称空资金流：attempt=%d view=%+v err=%v", attempt, view, err)
				}
			}
		})
	}
}

func TestFundFlowReviewCanceledFetchCanRetry(t *testing.T) {
	em := setupFlowReview(t)
	em.SetFetchForTest(func(ctx context.Context, _ string, _ map[string]string) ([]byte, int, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		return flowReviewBody(prevOpenTradeDate(time.Now().Format("2006-01-02"))), 200, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc := &MoodService{em: em}
	_, _ = svc.StockFundFlow(ctx, "cn", "600519", 90)
	view, err := svc.StockFundFlow(t.Context(), "cn", "600519", 90)
	if err != nil || view == nil || len(view.Days) != 1 {
		t.Fatalf("取消不能占住一小时冷却，使下一次正常读取丢失数据：view=%+v err=%v", view, err)
	}
}

func TestFundFlowReviewConcurrentReadWaitsForSharedFetch(t *testing.T) {
	em := setupFlowReview(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	em.SetFetchForTest(func(ctx context.Context, _ string, _ map[string]string) ([]byte, int, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
		return flowReviewBody(prevOpenTradeDate(time.Now().Format("2006-01-02"))), 200, nil
	})
	type result struct {
		view *StockFundFlowView
		err  error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	svc := &MoodService{em: em}
	go func() { view, err := svc.StockFundFlow(t.Context(), "cn", "600519", 90); first <- result{view, err} }()
	<-started
	go func() { view, err := svc.StockFundFlow(t.Context(), "cn", "600519", 90); second <- result{view, err} }()
	var secondResult *result
	select {
	case out := <-second:
		secondResult = &out
		t.Errorf("共享请求尚未完成，第二个读取提前给出结果：%+v", out)
	case <-time.After(80 * time.Millisecond):
	}
	unblock()
	one := <-first
	if secondResult == nil {
		out := <-second
		secondResult = &out
	}
	for _, out := range []result{one, *secondResult} {
		if out.err != nil || out.view == nil || len(out.view.Days) != 1 {
			t.Errorf("并发读取应取得真实数据：%+v", out)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("并发首访只能发起一次上游请求：%d", calls.Load())
	}
}

func TestFundFlowReviewIgnoresFutureRows(t *testing.T) {
	em := setupFlowReview(t)
	_ = em
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.Local)
	for _, date := range []string{"2026-09-08", "2026-09-09", "2026-09-10"} {
		if err := common.DB.Create(&model.FundFlowDaily{Market: "cn", Symbol: "600519", TradeDate: date, MainNet: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	probe := inspectStockFundFlow("cn", "600519", now)
	if len(probe.Rows) != 1 || probe.Rows[0].TradeDate != "2026-09-08" {
		t.Fatalf("盘中不能消费当日未终态或未来的资金流行：%+v", probe.Rows)
	}
}
