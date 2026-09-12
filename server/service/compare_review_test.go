package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type compareReviewAdapter struct {
	quoteTime time.Time
	bars      []datasource.Bar
}

func (compareReviewAdapter) Name() string { return "compare-review" }
func (a compareReviewAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	return &datasource.Quote{Symbol: symbol, Market: market, Name: "对比样本", Price: 4.037, DataTime: a.quoteTime}, nil
}
func (a compareReviewAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	if len(a.bars) == 0 {
		return nil, datasource.ErrNoData
	}
	return append([]datasource.Bar(nil), a.bars...), nil
}

// 经真实 Compare 采集、序列化和本地模型 HTTP 请求核验，缺少历史不能伪造零涨幅或均线位置。
func TestCompareDoesNotInventTechnicalData(t *testing.T) {
	prompts := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var prompt strings.Builder
		for _, message := range request.Messages {
			prompt.WriteString(message.Content)
		}
		prompts <- prompt.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"依据现价的本地测试点评"},"finish_reason":"stop"}],"usage":{"total_tokens":10}}`))
	}))
	t.Cleanup(upstream.Close)
	const userID int64 = 1120
	seedReportEnv(t, userID, upstream.URL)
	quoteTime := time.Now().In(time.Local)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: quoteTime.AddDate(0, 0, -1).Format("2006-01-02"), IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	if quoteTime.Hour()*60+quoteTime.Minute() < sessionQuoteReadyMin {
		previous := quoteTime.AddDate(0, 0, -1)
		quoteTime = time.Date(previous.Year(), previous.Month(), previous.Day(), 15, 0, 0, 0, time.Local)
	}
	for _, sample := range []struct {
		count int
		old   bool
	}{{0, false}, {10, false}, {21, false}, {60, true}} {
		t.Run(fmt.Sprintf("bars_%d_old_%v", sample.count, sample.old), func(t *testing.T) {
			end := quoteTime
			if sample.old {
				end = end.AddDate(0, 0, -3)
			}
			bars := wideGenBars(wideGenDates(sample.count, end), 4.037)
			svc := NewCompareService(NewMarketService(datasource.NewManagerWithAdapters(compareReviewAdapter{quoteTime, bars})), NewLLMService())
			result, err := svc.Compare(t.Context(), userID, true, CompareRequest{
				Symbols: []CompareSymbol{{Symbol: "510300", Market: "cn"}, {Symbol: "510500", Market: "cn"}}, WithAI: true,
			})
			if err != nil || result.AIComment == "" || len(result.Rows) != 2 {
				t.Fatalf("实时报价对比仍应完成：%+v %v", result, err)
			}
			prompt := <-prompts
			for _, row := range result.Rows {
				if !row.QuoteOK {
					t.Fatal("测试必须保持 fresh 报价，独立核验历史")
				}
				raw, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatal(err)
				}
				if row.Price != 4.037 || !strings.Contains(prompt, "4.037") {
					t.Errorf("ETF 价格不能截为两位：price=%v prompt=%s", row.Price, prompt)
				}
				if sample.count == 21 && !sample.old {
					if body["change_pct_5d"] != float64(0) || body["change_pct_20d"] != float64(0) || body["score"] == nil || body["above_ma20"] != true {
						t.Errorf("完整持平历史应保留真实 0%% 与均线位置：%s", raw)
					}
					if body["bars_as_of"] != end.Format("2006-01-02") {
						t.Errorf("历史必须标注独立截至日期：%s", raw)
					}
					continue
				}
				for _, key := range []string{"change_pct_20d", "above_ma20", "score"} {
					if body[key] != nil {
						t.Errorf("历史不足或过旧时 %s 应未知：%s", key, raw)
					}
				}
				if body["technical_note"] == nil || body["technical_note"] == "" {
					t.Errorf("必须说明指标缺口：%s", raw)
				}
				if strings.Contains(prompt, "近20日0.00%") || strings.Contains(prompt, "MA20=0.00") || strings.Contains(prompt, "位于MA20下方") {
					t.Errorf("不能向模型输入虚假技术结论：%s", prompt)
				}
				if sample.count == 0 || sample.old {
					if body["change_pct_5d"] != nil || body["ma5"] != nil {
						t.Errorf("缺失或过旧历史不能参与短期指标：%s", raw)
					}
				} else if body["change_pct_5d"] != float64(0) || body["ma5"] == nil {
					t.Errorf("十根历史的短期指标仍可用：%s", raw)
				}
			}
		})
	}
}
