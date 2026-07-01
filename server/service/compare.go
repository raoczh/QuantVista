package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

// CompareService 个股横向对比：多只股票并排比较行情与技术指标，可选 AI 一句话点评。
// 纯读操作，不落库；AI 点评复用 ai_client + 配额熔断。
type CompareService struct {
	market *MarketService
	llm    *LLMService
}

func NewCompareService(market *MarketService, llm *LLMService) *CompareService {
	return &CompareService{market: market, llm: llm}
}

const (
	compareMinSymbols = 2
	compareMaxSymbols = 6
)

// CompareRow 单只标的的对比指标。
type CompareRow struct {
	Symbol       string  `json:"symbol"`
	Market       string  `json:"market"`
	Name         string  `json:"name"`
	QuoteOK      bool    `json:"quote_ok"`
	Price        float64 `json:"price"`
	ChangePct    float64 `json:"change_pct"`
	Amount       float64 `json:"amount"`
	MA5          float64 `json:"ma5"`
	MA10         float64 `json:"ma10"`
	MA20         float64 `json:"ma20"`
	PeriodHigh   float64 `json:"period_high"`
	PeriodLow    float64 `json:"period_low"`
	ChangePct5d  float64 `json:"change_pct_5d"`
	ChangePct20d float64 `json:"change_pct_20d"`
	AbovMA20     bool    `json:"above_ma20"` // 现价是否站上 MA20
	Error        string  `json:"error"`
}

// CompareRequest 对比入参。
type CompareRequest struct {
	Symbols     []CompareSymbol `json:"symbols"`
	WithAI      bool            `json:"with_ai"`
	LLMConfigID int64           `json:"llm_config_id"`
}

type CompareSymbol struct {
	Symbol string `json:"symbol"`
	Market string `json:"market"`
}

// CompareResult 对比结果 + 可选 AI 点评。
type CompareResult struct {
	Rows      []CompareRow `json:"rows"`
	AIComment string       `json:"ai_comment"`
	Note      string       `json:"note"`
}

// Compare 并发采集各标的指标，去重后组装；WithAI 时追加一句话点评。
func (s *CompareService) Compare(ctx context.Context, userID int64, allowPrivate bool, req CompareRequest) (*CompareResult, error) {
	// 规整 + 去重。
	seen := map[string]bool{}
	refs := make([]CompareSymbol, 0, len(req.Symbols))
	for _, cs := range req.Symbols {
		symbol, market, err := normalizeSymbolMarket(cs.Symbol, cs.Market)
		if err != nil {
			continue
		}
		key := market + ":" + symbol
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, CompareSymbol{Symbol: symbol, Market: market})
	}
	if len(refs) < compareMinSymbols {
		return nil, fmt.Errorf("请至少提供 %d 只有效股票进行对比", compareMinSymbols)
	}
	if len(refs) > compareMaxSymbols {
		refs = refs[:compareMaxSymbols]
	}

	// 并发采集各标的指标。
	rows := make([]CompareRow, len(refs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ref CompareSymbol) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = s.buildRow(ctx, ref.Market, ref.Symbol)
		}(i, ref)
	}
	wg.Wait()

	res := &CompareResult{Rows: rows}

	// 可选 AI 一句话点评。
	if req.WithAI {
		comment, note := s.aiComment(ctx, userID, allowPrivate, req.LLMConfigID, rows)
		res.AIComment = comment
		if note != "" {
			res.Note = note
		}
	}
	return res, nil
}

// buildRow 采集单只标的的对比指标（行情 + 技术指标）。
func (s *CompareService) buildRow(ctx context.Context, market, symbol string) CompareRow {
	row := CompareRow{Symbol: symbol, Market: market}
	q, err := s.market.GetQuote(ctx, market, symbol)
	if err != nil {
		if errors.Is(err, datasource.ErrSymbolInvalid) {
			row.Error = "代码无效"
		} else {
			row.Error = "行情暂不可用"
		}
		return row
	}
	row.Name = q.Name
	row.QuoteOK = true
	row.Price = round2(q.Price)
	row.ChangePct = round2(q.ChangePct)
	row.Amount = round2(q.Amount)

	bars, berr := s.market.GetDailyBars(ctx, market, symbol, 60)
	if berr == nil && len(bars) > 0 {
		closes := make([]float64, len(bars))
		for i, b := range bars {
			closes[i] = b.Close
		}
		if v, ok := movingAverage(closes, 5); ok {
			row.MA5 = v
		}
		if v, ok := movingAverage(closes, 10); ok {
			row.MA10 = v
		}
		if v, ok := movingAverage(closes, 20); ok {
			row.MA20 = v
		}
		hi, lo := bars[0].High, bars[0].Low
		for _, b := range bars {
			if b.High > hi {
				hi = b.High
			}
			if b.Low < lo && b.Low > 0 {
				lo = b.Low
			}
		}
		row.PeriodHigh = round2(hi)
		row.PeriodLow = round2(lo)
		row.ChangePct5d = changeOverN(closes, 5)
		row.ChangePct20d = changeOverN(closes, 20)
		row.AbovMA20 = row.MA20 > 0 && row.Price >= row.MA20
	}
	return row
}

// changeOverN 近 N 个交易日涨跌幅（末值相对 N 根前收盘）。
func changeOverN(closes []float64, n int) float64 {
	if len(closes) <= n {
		return 0
	}
	prev := closes[len(closes)-1-n]
	if prev == 0 {
		return 0
	}
	return round2((closes[len(closes)-1] - prev) / prev * 100)
}

// aiComment 生成一段横向对比点评。返回（点评, 说明）；失败或配额不足时点评为空、说明解释原因。
func (s *CompareService) aiComment(ctx context.Context, userID int64, allowPrivate bool, llmConfigID int64, rows []CompareRow) (string, string) {
	cfg, apiKey, err := s.llm.ResolveForUse(userID, llmConfigID)
	if err != nil {
		return "", "AI 点评不可用：" + err.Error()
	}
	quota, err := s.getQuota(userID)
	if err == nil && quota.TokenLimit > 0 && quota.TokenUsed >= quota.TokenLimit {
		return "", "AI 配额已用尽，仅展示指标对比"
	}

	// 只把有行情的标的喂给模型。
	var b strings.Builder
	b.WriteString("请对下列股票做一次简明的横向对比点评（150 字以内，一段话）：指出相对强弱、趋势与均线位置差异、谁更值得关注及其理由；只依据给出的行情与技术指标，不得虚构财务/新闻；这是研究参考，不构成投资建议，末尾一句风险提示。\n\n数据：\n")
	valid := 0
	for _, r := range rows {
		if !r.QuoteOK {
			continue
		}
		valid++
		fmt.Fprintf(&b, "- %s(%s)：现价%.2f 涨跌%.2f%% 近5日%.2f%% 近20日%.2f%% MA20=%.2f %s，区间[%.2f,%.2f]\n",
			nameOr(r), r.Symbol, r.Price, r.ChangePct, r.ChangePct5d, r.ChangePct20d, r.MA20,
			aboveText(r.AbovMA20), r.PeriodLow, r.PeriodHigh)
	}
	if valid < compareMinSymbols {
		return "", "有效行情不足，无法生成 AI 点评"
	}

	res, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: "你是严谨的证券研究助理，输出仅供研究参考，不构成投资建议。"},
			{Role: "user", Content: b.String()},
		},
		JSONMode: false, AllowPrivate: allowPrivate,
	})
	if err != nil {
		return "", "AI 点评生成失败：" + err.Error()
	}
	if res.Usage.TotalTokens > 0 {
		s.addUsage(userID, res.Usage.TotalTokens)
	}
	return strings.TrimSpace(res.Content), ""
}

func nameOr(r CompareRow) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Symbol
}
func aboveText(above bool) string {
	if above {
		return "站上MA20"
	}
	return "位于MA20下方"
}

func (s *CompareService) getQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *CompareService) addUsage(userID int64, tokens int) {
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(map[string]any{
		"token_used":    gorm.Expr("token_used + ?", tokens),
		"request_count": gorm.Expr("request_count + 1"),
	})
}
