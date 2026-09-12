package service

import (
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestScoreDoesNotPersistMissingOrStaleHistory(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	if quoteTime.Hour()*60+quoteTime.Minute() < sessionQuoteReadyMin {
		previous := quoteTime.AddDate(0, 0, -1)
		quoteTime = time.Date(previous.Year(), previous.Month(), previous.Day(), 15, 0, 0, 0, time.Local)
	}
	for i, sample := range []struct {
		count int
		old   bool
	}{{0, false}, {10, false}, {21, false}, {60, true}} {
		end := quoteTime
		if sample.old {
			end = end.AddDate(0, 0, -3)
		}
		symbol := fmt.Sprintf("51030%d", i)
		bars := wideGenBars(wideGenDates(sample.count, end), 4.037)
		svc := NewScoreService(NewMarketService(datasource.NewManagerWithAdapters(compareReviewAdapter{quoteTime, bars})))
		view, err := svc.Score(t.Context(), "cn", symbol)
		var rows []model.StockScore
		if err := common.DB.Where("symbol = ?", symbol).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if sample.count == 21 && !sample.old {
			if err != nil || view == nil || view.Price != 4.037 || len(rows) != 1 || rows[0].Price != 4.037 {
				t.Errorf("正常评分快照须保留原单价：view=%+v rows=%+v err=%v", view, rows, err)
			}
			continue
		}
		if err == nil || view != nil || len(rows) != 0 {
			t.Errorf("不可用历史不能生成并落库当日评分 count=%d old=%v：view=%+v rows=%+v err=%v", sample.count, sample.old, view, rows, err)
		}
	}
}

func TestTechnicalDecisionsPreserveFractionalPrice(t *testing.T) {
	closes := make([]float64, 20)
	for i := range closes {
		closes[i] = 4.037
	}
	// 现价高于均线，不能因均值先四舍五入成 4.04 而发出跌破提醒。
	rule := model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpLTE, Period: 20}
	if fired, _, text := evaluateAlert(rule, alertEval{Price: 4.038, Closes: closes}); fired {
		t.Errorf("截断第三位导致错误跌破提醒：%s", text)
	}
	if got := trendScore(4.038, closes); got != 60 {
		t.Errorf("高于三条相等均线时趋势分应为 60，得到 %v", got)
	}
	bars := wideGenBars(wideGenDates(20, time.Now()), 4.037)
	bars[len(bars)-1].Close, bars[len(bars)-1].High = 4.038, 4.038
	values := computeWideRow("600001", wideStockMeta{}, bars)
	if values[factorIndex["close"]] != 4.038 || values[factorIndex["above_ma20"]] != 1 {
		t.Fatalf("因子宽表不能重新将价格和均线截成两位：close=%v above_ma20=%v", values[factorIndex["close"]], values[factorIndex["above_ma20"]])
	}
}

func TestAnalysisHistoryDoesNotInventChange(t *testing.T) {
	for _, count := range []int{10, 21} {
		bars := wideGenBars(wideGenDates(count, time.Now()), 4.037)
		tech := computeTechnicals(bars)
		change, exists := tech["change_pct_20d"]
		if count < 21 && exists {
			t.Errorf("十根日线不能给模型注入近 20 日零涨幅：%+v", tech)
		}
		if count == 21 && (!exists || change != float64(0)) {
			t.Errorf("满窗持平历史必须保留真实零涨幅：%+v", tech)
		}
		compact := compactBars(bars, 1)
		if tech["period_high"] != 4.037 || tech["period_low"] != 4.037 || compact[0]["c"] != 4.037 {
			t.Errorf("模型的价格证据不能先截为两位：tech=%+v bar=%+v", tech, compact[0])
		}
	}
}
