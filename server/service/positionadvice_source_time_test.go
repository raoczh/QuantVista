package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestPositionAdviceKeepsQuoteSourceTimeThroughModelRoundTrip(test *testing.T) {
	requests := make(chan []positionAdviceRow, 2)
	modelServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		var rows []positionAdviceRow
		for _, message := range payload.Messages {
			if message.Role != "user" {
				continue
			}
			if start := strings.Index(message.Content, "["); start >= 0 {
				_ = json.Unmarshal([]byte(message.Content[start:]), &rows)
			}
		}
		if len(rows) != 1 {
			http.Error(response, "expected one position input", http.StatusBadRequest)
			return
		}
		requests <- rows
		content, _ := json.Marshal(map[string]any{"advices": []PositionAdvice{{
			PositionID: rows[0].PositionID, Symbol: rows[0].Symbol,
			Verdict: PositionVerdictHold, Reason: "Price 11.00 remains above cost 10.00", Invalidation: "Price below 10.00",
			QuoteAsOf: "2099-01-01 10:00",
		}}})
		_ = json.NewEncoder(response).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
	defer modelServer.Close()
	const userID int64 = 919
	seedReportEnv(test, userID, modelServer.URL)
	now := time.Now().In(datasource.ChinaMarketLocation())
	now = time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location())
	calendar := model.TradingCalendar{Market: "cn", TradeDate: now.Format(time.DateOnly), IsOpen: true}
	if err := common.DB.Where("market = ? AND trade_date = ?", calendar.Market, calendar.TradeDate).FirstOrCreate(&calendar).Error; err != nil {
		test.Fatal(err)
	}
	seedHoldingWithPeak(test, userID, "600919", "Quote time fixture", 10, 100, 11, now.AddDate(0, 0, -1).Format(time.DateOnly))
	sourceTime := now.Add(-3 * time.Minute)
	market := NewMarketService(datasource.NewManagerWithAdapters(sourceQuoteReviewAdapter{source: "source-time-fixture", observed: sourceTime}))
	market.quoteNow = func() time.Time { return now }
	service := NewPositionAdviceService(NewPositionService(market), NewLLMService())
	result, err := service.Advise(test.Context(), userID, true, PositionAdviceRequest{})
	if err != nil || result == nil || len(result.Advices) != 1 {
		test.Fatalf("position advice fixture failed: %+v %v", result, err)
	}
	input := <-requests
	expectedSourceTime := sourceTime.In(time.Local).Format("2006-01-02 15:04")
	if input[0].QuoteAsOf != expectedSourceTime || result.Advices[0].QuoteAsOf != expectedSourceTime {
		test.Fatalf("source time was lost or trusted from model: input=%+v output=%+v", input[0], result.Advices[0])
	}
	if result.GeneratedAt == "" || result.GeneratedAt == result.Advices[0].QuoteAsOf {
		test.Fatalf("generation and source clocks were conflated: %+v", result)
	}
}
