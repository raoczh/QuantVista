package service

import (
	"context"
	"errors"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestHistoricalUniverseReadFailureStopsEvaluation(t *testing.T) {
	for _, kind := range []string{"backtest", "walk_forward", "factor_ic"} {
		t.Run(kind, func(t *testing.T) {
			setupTestDB(t)
			if err := common.DB.Where("1=1").Delete(&model.StockUniverseDaily{}).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Where("1=1").Delete(&model.StockUniverseDaily{}) })
			var axis []string
			if kind == "backtest" {
				axis = seedBacktestDB(t)
			} else {
				axis = seedWFFixture(t)
			}
			if err := common.DB.Create(&model.StockUniverseDaily{TradeDate: axis[0], Symbol: "600001", Market: "cn", IsST: true}).Error; err != nil {
				t.Fatal(err)
			}
			fault := errors.New("本地注入历史 ST 名单查询失败")
			injected := false
			const callback = "review_historical_universe_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "stock_universe_dailies" {
					injected = true
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			var err error
			switch kind {
			case "backtest":
				tree := allOf(leafV("close", ">", 5))
				svc := &BacktestService{benchFn: func(context.Context) []datasource.Bar { return fakeBench(axis) }}
				_, err = svc.Run(context.Background(), 1, BacktestRequest{Tree: &tree, LookbackDays: 10, SignalCount: 2, HoldDays: []int{2}})
			case "walk_forward":
				_, err = RunWalkForward(context.Background(), nil)
			case "factor_ic":
				_, err = RunFactorIC(context.Background(), nil)
			}
			if !injected || !errors.Is(err, fault) {
				t.Fatalf("不能把 ST 名单读取失败当成当天没有 ST 股票：injected=%v err=%v", injected, err)
			}
		})
	}
}
