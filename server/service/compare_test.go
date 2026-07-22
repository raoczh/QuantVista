package service

import (
	"context"
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

type compareAsyncAdapter struct{}

func (compareAsyncAdapter) Name() string { return "compare-async-test" }

func (compareAsyncAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	price := 10.0
	if symbol == "000002" {
		price = 12
	}
	return &datasource.Quote{
		Symbol: symbol, Market: market, Name: "T" + symbol, Price: price,
		PrevClose: price - 0.2, ChangePct: 1.2, Amount: 1e8, Source: "test", DataTime: time.Now(),
	}, nil
}

func (compareAsyncAdapter) GetDailyBars(_ context.Context, _, symbol string, limit int) ([]datasource.Bar, error) {
	if limit <= 0 {
		limit = 30
	}
	bars := make([]datasource.Bar, limit)
	base := 10.0
	if symbol == "000002" {
		base = 12
	}
	for i := range bars {
		closePrice := base + float64(i)/100
		bars[i] = datasource.Bar{TradeDate: time.Now().AddDate(0, 0, i-limit+1).Format("2006-01-02"), Open: closePrice - 0.1,
			High: closePrice + 0.2, Low: closePrice - 0.2, Close: closePrice, Volume: 10000, Source: "test"}
	}
	return bars, nil
}

// TestChangeOverN 近 N 日涨跌幅计算与边界。
func TestChangeOverN(t *testing.T) {
	closes := []float64{10, 10.5, 11, 10.8, 12} // 末值 12
	// 近 4 日：相对 closes[len-1-4]=closes[0]=10 → (12-10)/10*100 = 20。
	if got := changeOverN(closes, 4); got != 20 {
		t.Fatalf("近4日应 20%%，得到 %v", got)
	}
	// 近 2 日：相对 closes[2]=11 → (12-11)/11*100 ≈ 9.09。
	if got := changeOverN(closes, 2); got != 9.09 {
		t.Fatalf("近2日应 9.09%%，得到 %v", got)
	}
	// 数据不足：N >= len → 0。
	if got := changeOverN(closes, 5); got != 0 {
		t.Fatalf("数据不足应 0，得到 %v", got)
	}
	// prev 为 0 → 0（防除零）。
	if got := changeOverN([]float64{0, 5, 8}, 2); got != 0 {
		t.Fatalf("前值为0应 0，得到 %v", got)
	}
}

// TestAboveText / nameOr 辅助文案。
func TestCompareHelpers(t *testing.T) {
	if aboveText(true) != "站上MA20" || aboveText(false) != "位于MA20下方" {
		t.Fatalf("aboveText 文案错误")
	}
	if nameOr(CompareRow{Symbol: "600000"}) != "600000" {
		t.Fatalf("无名称应回退 symbol")
	}
	if nameOr(CompareRow{Symbol: "600000", Name: "浦发银行"}) != "浦发银行" {
		t.Fatalf("有名称应用名称")
	}
}

func TestNormalizeCompareRequestKeepsTruncationNote(t *testing.T) {
	req := CompareRequest{Symbols: []CompareSymbol{
		{Symbol: "000001", Market: "cn"}, {Symbol: "000002", Market: "cn"},
		{Symbol: "000003", Market: "cn"}, {Symbol: "000004", Market: "cn"},
		{Symbol: "000005", Market: "cn"}, {Symbol: "000006", Market: "cn"},
		{Symbol: "000007", Market: "cn"}, {Symbol: "000001", Market: "cn"},
	}}
	refs, note, err := normalizeCompareRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != compareMaxSymbols || !strings.Contains(note, "已忽略多余标的") {
		t.Fatalf("超过上限应截断并保留提示: len=%d note=%q", len(refs), note)
	}
}

// TestCompareRowLine ETF 行不得携带估值段（腾讯源对基金 PE/PB 为 0，喂给模型是噪声），
// 且须显式标注基金身份；个股行有估值时正常拼入。
func TestCompareRowLine(t *testing.T) {
	fund := CompareRow{Symbol: "510300", Name: "沪深300ETF", QuoteOK: true, Price: 4.037, IsFund: true}
	line := compareRowLine(fund)
	if !strings.Contains(line, "ETF/场内基金") {
		t.Fatalf("基金行应带标注：%s", line)
	}
	if strings.Contains(line, "PE-TTM") {
		t.Fatalf("基金行不应携带估值段：%s", line)
	}
	stock := CompareRow{Symbol: "600000", Name: "浦发银行", QuoteOK: true, Price: 8.5, ValuationOK: true, PETTM: 5.2, PB: 0.4, TotalCap: 2500e8, TurnoverRate: 0.8}
	sline := compareRowLine(stock)
	if !strings.Contains(sline, "PE-TTM=5.20") || strings.Contains(sline, "ETF/场内基金") {
		t.Fatalf("个股行应带估值段且无基金标注：%s", sline)
	}
}

// TestCompareAsyncTask 带 AI 的对比只在 HTTP 阶段建 processing 任务，
// 行情采集与 LLM 点评在后台完成，终态 result 仍是原 CompareResult 结构。
func TestCompareAsyncTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"A 综合分更高，但仍需注意市场风险。"}}],"usage":{"total_tokens":80}}`))
	}))
	defer srv.Close()

	const userID int64 = 12021
	resetAsyncLLMTasks(t)
	seedParseStrategyEnv(t, userID, srv.URL)
	common.DB.Where("market = ? AND trade_date = ?", "cn", time.Now().Format("2006-01-02")).
		Delete(&model.TradingCalendar{})
	if err := common.DB.Create(&model.TradingCalendar{
		Market: "cn", TradeDate: time.Now().Format("2006-01-02"), IsOpen: true,
	}).Error; err != nil {
		t.Fatalf("造交易日历失败: %v", err)
	}
	t.Cleanup(func() {
		common.DB.Where("market = ? AND trade_date = ?", "cn", time.Now().Format("2006-01-02")).
			Delete(&model.TradingCalendar{})
	})

	market := NewMarketService(datasource.NewManagerWithAdapters(compareAsyncAdapter{}))
	svc := NewCompareService(market, NewLLMService())
	task, err := svc.CompareAsync(userID, true, CompareRequest{Symbols: []CompareSymbol{
		{Symbol: "000001", Market: "cn"}, {Symbol: "000002", Market: "cn"},
	}, WithAI: true})
	if err != nil {
		t.Fatalf("建对比任务失败: %v", err)
	}
	if task.Kind != "compare" || task.Status != model.LLMTaskStatusProcessing {
		t.Fatalf("应立即返回 compare processing 任务: %+v", task)
	}
	done := waitAsyncLLMTask(t, userID, task.ID)
	if done.Status != model.LLMTaskStatusSuccess {
		t.Fatalf("对比任务应成功: %+v", done)
	}
	var got CompareResult
	if err := json.Unmarshal(done.Result, &got); err != nil {
		t.Fatalf("解码对比结果失败: %v; raw=%s", err, done.Result)
	}
	if len(got.Rows) != 2 || got.AIComment == "" {
		t.Fatalf("后台结果应保留对比行和 AI 点评: %+v", got)
	}
}
