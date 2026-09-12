package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLHindsightAndCalibrationKeepOnePriceBasis(t *testing.T) {
	for _, kind := range []string{"hindsight", "calibration"} {
		t.Run(kind, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.AnalysisRecord{}, &model.DailyBar{})
			seedCalibBars(t, "600901", 20, 1)
			id := seedCalibAnalysis(t, "600901", model.AnalysisRatingBullish, 80, "high", "")
			read, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			const callback = "review_hindsight_price_basis"
			if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "daily_bars" {
					once.Do(func() {
						close(read)
						select {
						case <-release:
						case <-time.After(5 * time.Second):
							tx.AddError(errors.New("价格读取未释放"))
						}
					})
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
			type result struct {
				ret float64
				hit bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				if kind == "hindsight" {
					view, err := (&AnalysisService{}).Hindsight(context.Background(), 1, id, 0, 0)
					if err != nil || view == nil || view.Returns["d20"] == nil {
						done <- result{err: errors.Join(err, errors.New("缺少后验收益"))}
						return
					}
					done <- result{ret: view.Returns["d20"].ReturnPct, hit: view.RatingHit != nil && *view.RatingHit}
					return
				}
				view, err := buildAnalysisCalibReport(context.Background())
				if err != nil || view == nil || len(view.Reliability) != 1 {
					done <- result{err: errors.Join(err, errors.New("缺少校准样本"))}
					return
				}
				done <- result{ret: view.Reliability[0].AvgNetPct, hit: view.Reliability[0].HitRatePct == 100}
			}()
			select {
			case <-read:
			case r := <-done:
				t.Fatalf("读取提前结束：%v", r.err)
			case <-time.After(5 * time.Second):
				t.Fatal("未进入基准价格查询")
			}
			// 一次原子重锚：整个序列由 10→12 变为 5→6，真实相对收益仍为 20%。
			if err := db.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", "600901").
				Updates(map[string]any{"open": gorm.Expr("open / 2"), "high": gorm.Expr("high / 2"),
					"low": gorm.Expr("low / 2"), "close": gorm.Expr("close / 2")}).Error; err != nil {
				t.Fatal(err)
			}
			unblock()
			select {
			case r := <-done:
				if r.err != nil || r.ret != 20 || !r.hit {
					t.Fatalf("不能将重锚前基准 10 与重锚后终值 6 相除：ret=%v hit=%v err=%v", r.ret, r.hit, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("后验读取没有完成")
			}
		})
	}
}
