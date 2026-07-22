package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quantvista/datasource"
	"quantvista/model"
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
	// comparePromptVersion 对比 AI 点评的内联提示词版本（P0-2 修复批新设）：aiComment 的
	// system/user 提示词为固定内联文本，改措辞须递增本版本——审计与 trace 关联凭它归因。
	// c1: 初版（横向对比点评 180 字约束 + 数值引用纪律 + 风险提示）。
	comparePromptVersion = "c1"
)

// CompareRow 单只标的的对比指标。行情时效契约：QuoteOK 仅在取到 fresh 行情时为
// true（stale/unknown 的最近已知价仍填 Price 供表格展示，但带 FreshnessStatus/
// QuoteAsOf/Error 标注，且不参与评分与 AI 点评）。
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
	Score        float64 `json:"score"`      // 综合评分 0-100
	ScoreLabel   string  `json:"score_label"`
	ValuationOK  bool    `json:"valuation_ok"`  // 估值快照是否可得且时效有效（腾讯免费源 best-effort）
	IsFund       bool    `json:"is_fund"`       // ETF/场内基金（无个股估值指标，估值段跳过）
	PETTM        float64 `json:"pe_ttm"`        // 市盈率 TTM（负值=亏损）
	PB           float64 `json:"pb"`            // 市净率
	TotalCap     float64 `json:"total_cap"`     // 总市值（元）
	TurnoverRate float64 `json:"turnover_rate"` // 换手率 %
	VolumeRatio  float64 `json:"volume_ratio"`  // 量比
	IsST         bool    `json:"is_st"`
	Error        string  `json:"error"`

	QuoteAsOf       string `json:"quote_as_of,omitempty"`      // 行情数据源时刻
	FreshnessStatus string `json:"freshness_status,omitempty"` // fresh | stale | unknown
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
	Rows           []CompareRow   `json:"rows"`
	AIComment      string         `json:"ai_comment"`
	AICommentCheck *evidenceCheck `json:"ai_comment_check,omitempty"` // 服务端回填：AI 点评引用数字与各行指标的核验
	Note           string         `json:"note"`
	// AIRefusalCode AI 点评未生成时的机读拒答码（行情不足、配额/配置、调用/完整性等，
	// 空=已生成或未请求 AI）；Note 保持人类可读说明。
	AIRefusalCode string `json:"ai_refusal_code,omitempty"`
	// AITraceID P0-2 调用追溯 ID（对比不落库，随响应回传；管理端 llm-calls 按 trace 可查）。
	AITraceID string `json:"ai_trace_id,omitempty"`
	// AI 点评实际使用的 LLM（对比不落库，随响应回传供前端展示；无点评时为零值）。
	AIConfigID int64  `json:"ai_llm_config_id,omitempty"`
	AIProvider string `json:"ai_provider,omitempty"`
	AIModel    string `json:"ai_model,omitempty"`
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
	truncNote := ""
	if len(refs) > compareMaxSymbols {
		truncNote = fmt.Sprintf("最多同时对比 %d 只，已忽略多余标的；", compareMaxSymbols)
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

	res := &CompareResult{Rows: rows, Note: truncNote}

	// 可选 AI 一句话点评。
	if req.WithAI {
		comment, note, refusalCode, usedCfg, aiTrace := s.aiComment(ctx, userID, allowPrivate, req.LLMConfigID, rows)
		res.AIComment = comment
		res.AIRefusalCode = refusalCode
		res.AITraceID = aiTrace
		if comment != "" {
			// 证据核验：点评引用的数字与全部对比行的指标值域比对（点评无 K 线明细，全量收集即可）。
			res.AICommentCheck = verifyEvidenceLabeled(
				[]evidenceSection{{Module: "AI点评", Text: comment}},
				snapshotLabeledValues(rows, nil))
			markKeySection(res.AICommentCheck, "AI点评")
		}
		if comment != "" && usedCfg != nil {
			res.AIConfigID = usedCfg.ID
			res.AIProvider = usedCfg.Provider
			res.AIModel = usedCfg.Model
		}
		if note != "" {
			res.Note = truncNote + note
		}
	}
	return res, nil
}

// buildRow 采集单只标的的对比指标（行情 + 技术指标）。
// fail-closed：走 GetFreshQuote，仅 fresh 记 QuoteOK（stale/unknown 的最近已知价
// 保留展示但带状态与截至时刻，不参与评分与 AI 点评——旧价对比会得出假强弱）。
func (s *CompareService) buildRow(ctx context.Context, market, symbol string) CompareRow {
	row := CompareRow{Symbol: symbol, Market: market}
	q, fi, err := s.market.GetFreshQuote(ctx, market, symbol)
	if err != nil || q == nil || q.Price <= 0 {
		if errors.Is(err, datasource.ErrSymbolInvalid) {
			row.Error = "代码无效"
		} else {
			row.Error = "行情暂不可用"
		}
		return row
	}
	row.Name = q.Name
	row.FreshnessStatus = fi.Status
	if !q.DataTime.IsZero() {
		row.QuoteAsOf = q.DataTime.In(time.Local).Format("2006-01-02 15:04")
	}
	row.Price = round2(q.Price)
	row.ChangePct = round2(q.ChangePct)
	row.Amount = round2(q.Amount)
	if fi.Status != freshStatusFresh {
		if fi.Status == freshStatusUnknown {
			row.Error = "行情时效无法核验（该市场无交易日历），未参与对比结论"
		} else {
			row.Error = "行情已过期（截至 " + orStr(row.QuoteAsOf, "未知时间") + "），未参与对比结论"
		}
		return row
	}
	row.QuoteOK = true

	// 估值快照（腾讯免费源，best-effort：拿不到只是该维度缺席）。
	// ETF/场内基金无个股估值指标（腾讯源 PE/PB 为空→0），跳过以免把全 0 当真值
	// 喂给 AI 点评——与 analysis_context.go 的 ETF 分支同口径。
	// 估值 DataTime 须 fresh：过期估值的换手/量比会当作今日值混进对比。
	if market == "cn" && isCNFund(symbol) {
		row.IsFund = true
	} else if v, verr := s.market.GetValuation(ctx, market, symbol); verr == nil &&
		s.market.QuoteFreshnessOf(market, v.DataTime).Status == freshStatusFresh {
		row.ValuationOK = true
		row.PETTM = round2(v.PETTM)
		row.PB = round2(v.PB)
		row.TotalCap = round2(v.TotalCap)
		row.TurnoverRate = round2(v.TurnoverRate)
		row.VolumeRatio = round2(v.VolumeRatio)
		row.IsST = v.IsST
	}

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
		// 综合评分（复用评分引擎，行情+技术指标口径一致）。
		sc := computeScore(q.Price, bars)
		row.Score = sc.Total
		row.ScoreLabel = sc.Label
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

// aiComment 生成一段横向对比点评。返回（点评, 说明, 机读拒答码, 实际使用的 LLM 配置, trace_id）；
// 失败或配额不足时点评为空、说明解释原因、code 供前端程序化分支、配置为 nil。
// trace_id 仅在实际发起过 LLM 调用时非空（P0-2；对比不落库，随响应回传）。
func (s *CompareService) aiComment(ctx context.Context, userID int64, allowPrivate bool, llmConfigID int64, rows []CompareRow) (string, string, string, *model.LLMConfig, string) {
	// 数据充分性是业务安全门，应先于配置和配额解析。否则同一批 fresh<2 的数据会因
	// 用户是否配置 LLM 而返回不同拒答码，掩盖真正的 fail-closed 原因。
	var b strings.Builder
	b.WriteString("请对下列股票做一次简明的横向对比点评（180 字以内，一段话）：指出相对强弱、趋势与均线位置差异、估值水位差异（如有 PE/PB 数据）、谁更值得关注及其理由。要求：只依据给出的数据，关键判断引用具体数值（如「A 综合分 78 高于 B 的 52」）；系统会程序化核对你引用的数字，与数据不符的会被标记展示给用户，故不得编造或凭印象填数；禁止使用你记忆中关于这些公司的信息，不得虚构财务明细/新闻；这是研究参考，不构成投资建议，末尾一句风险提示。\n\n数据（score 为本站五维技术评分 0-100，仅供参考锚点）：\n")
	valid := 0
	for _, r := range rows {
		if !r.QuoteOK {
			continue
		}
		valid++
		b.WriteString(compareRowLine(r))
		b.WriteString("\n")
	}
	if valid < compareMinSymbols {
		// P0-7 fail-closed：fresh 行情不足 2 只时对比结论无从谈起（旧价比强弱是假强弱），
		// 拒绝调 AI；stale 行已在表格带状态展示。
		return "", "有效行情不足，无法生成 AI 点评", RefusalFreshQuotesInsufficient, nil, ""
	}

	cfg, apiKey, err := s.llm.ResolveForUse(userID, llmConfigID)
	if err != nil {
		return "", "AI 点评不可用：" + err.Error(), RefusalLLMUnavailable, nil, ""
	}
	allowPrivate = llmAllowPrivate(allowPrivate, cfg) // 回退到管理员配置时按配置所有者放行内网
	if err := checkQuota(userID); err != nil {
		if code := RefusalCodeOf(err); code != "" {
			if code == RefusalQuotaExhausted {
				return "", "AI 次数配额已用尽，仅展示指标对比", code, nil, ""
			}
			return "", "AI 点评不可用：配额信息读取失败", code, nil, ""
		}
		return "", "AI 点评不可用：配额信息读取失败", RefusalQuotaUnavailable, nil, ""
	}

	// P0-2：对比无业务落库行，trace 随响应回传（管理端审计按 trace_id 可查本次调用）。
	// prompt 版本接 comparePromptVersion（内联提示词的稳定显式版本，改措辞须递增）。
	run := newLLMRun(newLLMTraceID(), "", "compare", "compare.free_text.v1", comparePromptVersion)
	messages := []chatMessage{
		{Role: "system", Content: "你是严谨的证券研究助理，输出仅供研究参考，不构成投资建议。"},
		{Role: "user", Content: b.String()},
	}
	run.hashData(b.String())
	run.hashPrompt(messages)
	res, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("compare", cfg.MaxTokens),
		Messages: messages,
		JSONMode: false, AllowPrivate: allowPrivate,
		Meta: run.chatMeta(userID, cfg, 1),
	})
	run.record(res, err)
	if err != nil {
		code := RefusalCodeOf(err)
		if code == "" {
			code = RefusalLLMCallFailed
		}
		return "", "AI 点评生成失败：" + err.Error(), code, nil, run.TraceID
	}
	if res.Usage.TotalTokens > 0 {
		consumeQuota(userID, res.Usage.TotalTokens, true)
	}
	return strings.TrimSpace(res.Content), "", "", cfg, run.TraceID
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

// compareRowLine 拼装单只标的的数据行供 AI 点评 prompt 使用（抽成纯函数便于单测）。
// ETF/场内基金行不带估值段并显式标注，防止模型臆测估值。
func compareRowLine(r CompareRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s(%s)：现价%.2f 涨跌%.2f%% 近5日%.2f%% 近20日%.2f%% MA20=%.2f %s，区间[%.2f,%.2f]",
		nameOr(r), r.Symbol, r.Price, r.ChangePct, r.ChangePct5d, r.ChangePct20d, r.MA20,
		aboveText(r.AbovMA20), r.PeriodLow, r.PeriodHigh)
	if r.QuoteAsOf != "" {
		fmt.Fprintf(&b, "（行情时点 %s）", r.QuoteAsOf)
	}
	if r.Score > 0 {
		fmt.Fprintf(&b, "，综合分%.0f(%s)", r.Score, r.ScoreLabel)
	}
	if r.ValuationOK {
		fmt.Fprintf(&b, "，PE-TTM=%.2f PB=%.2f 总市值%.0f亿 换手%.2f%%", r.PETTM, r.PB, r.TotalCap/1e8, r.TurnoverRate)
		if r.VolumeRatio > 0 {
			fmt.Fprintf(&b, " 量比%.2f", r.VolumeRatio)
		}
	}
	if r.IsFund {
		b.WriteString("，ETF/场内基金（无 PE/PB 个股估值指标，勿臆测估值）")
	}
	return b.String()
}
