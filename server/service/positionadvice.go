package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// D17 AI 持有 / 减仓 / 清仓建议。
//
// 与既有「持仓模块 AI 分析」的根本区别：那个是对**整个组合**的一段自由文本分析，
// 这个是**逐笔持仓**的封闭枚举结论 `hold | trim | exit` + 理由 + 失效条件。
// 用户要的不是「泛泛的个股分析」，是「这一笔，我到底该不该卖」。
//
// 纪律（与全链路一致，逐条对应反例测试）：
//   - **程序可算的量一律在 Go 算好喂进去**：成本、浮盈亏、持有交易日、回撤、
//     仓位占比——模型只做判断，不做算术（算术是它最容易错的部分）；
//   - **fail-closed**：无 fresh 行情的持仓不进请求（旧价上的割/守/补结论是有害的），
//     一笔都没有则整体拒答 insufficient_fresh_quotes；
//   - **结论进证据核验值域**：模型引用的数字与快照比对，编造的会被标记展示；
//   - **走统一 JobRun**：HTTP 仍返回兼容 llm_task id，旧前端轮询与深链保持可用。

const (
	// positionAdvicePromptVersion 建议 prompt 版本（改措辞/枚举语义必须递增，审计按它归因）。
	positionAdvicePromptVersion = "pa2"
	// positionAdviceJobTimeout 后台任务总预算。
	positionAdviceJobTimeout = 5 * time.Minute
	// positionAdviceMaxPositions 单次最多分析的持仓笔数（控上下文预算）。超出按
	// 「浮亏最深优先」截取并在 Notes 里如实声明丢了几笔——**绝不静默截断**。
	positionAdviceMaxPositions = 20

	// 结论枚举（封闭三值，服务端强校验，越界丢弃）。
	PositionVerdictHold = "hold" // 继续持有
	PositionVerdictTrim = "trim" // 减仓（部分兑现/降低风险敞口）
	PositionVerdictExit = "exit" // 清仓
)

// PositionAdviceService 持仓卖出决策建议。
type PositionAdviceService struct {
	position *PositionService
	llm      *LLMService
}

func NewPositionAdviceService(position *PositionService, llm *LLMService) *PositionAdviceService {
	svc := &PositionAdviceService{position: position, llm: llm}
	svc.registerDurableJobHandler()
	return svc
}

// PositionAdvice 单笔持仓的结论。
type PositionAdvice struct {
	// PositionID 是模型必须原样回传的逐持仓身份；服务端按 ID+symbol 双重校验。
	PositionID int64  `json:"position_id"`
	Symbol     string `json:"symbol"`
	Name       string `json:"name,omitempty"`
	// PositionType/Cost/Quantity 来自服务端持仓快照，模型输出中的同名字段一律覆盖。
	PositionType string  `json:"position_type"`
	Cost         float64 `json:"cost"`
	Quantity     float64 `json:"quantity"`
	// Verdict 封闭枚举 hold|trim|exit（服务端归一，非法值整条丢弃）。
	Verdict string `json:"verdict"`
	// Reason 结论理由（要求引用喂进去的具体数值）。
	Reason string `json:"reason"`
	// Invalidation 失效条件：什么情况下这个结论不再成立（对齐既有 kill_switches 纪律）。
	Invalidation string `json:"invalidation"`
}

// PositionAdviceResult 一次建议的完整结果（落兼容 llm_tasks.result_json，JobRun 只存引用）。
type PositionAdviceResult struct {
	Advices     []PositionAdvice `json:"advices"`
	GeneratedAt string           `json:"generated_at"`
	// Analyzed/Skipped 参与与被排除的持仓数；Notes 说明为什么被排除。
	Analyzed int      `json:"analyzed"`
	Skipped  int      `json:"skipped"`
	Notes    []string `json:"notes"`

	// EvidenceCheck 结论引用数字与快照的程序化核验（信任层标配）。
	EvidenceCheck *evidenceCheck `json:"evidence_check,omitempty"`

	// 实际使用的 LLM（不落业务表，随结果回传供前端标注）。
	LLMConfigID int64  `json:"llm_config_id,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	PromptVer   string `json:"prompt_version,omitempty"`
}

func stampPositionAdviceResult(res *PositionAdviceResult, now time.Time) {
	res.GeneratedAt = now.Format(time.RFC3339)
}

// PositionAdviceRequest 入参。
type PositionAdviceRequest struct {
	LLMConfigID int64 `json:"llm_config_id"`
	// Symbol 可选：只分析某一只（空=全部持仓）。
	Symbol string `json:"symbol"`
}

// normalizePositionVerdict 归一结论枚举（模型常输出 HOLD/持有/减持等变体）。
// 不认识的一律返回空串 → 整条丢弃。**绝不猜一个默认值**：猜 hold 会把
// 「模型没给出有效结论」伪装成「模型建议继续持有」。
func normalizePositionVerdict(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hold", "持有", "继续持有":
		return PositionVerdictHold
	case "trim", "减仓", "减持", "部分减仓":
		return PositionVerdictTrim
	case "exit", "清仓", "卖出", "全部卖出":
		return PositionVerdictExit
	}
	return ""
}

// PositionVerdictLabel 结论的中文标签（后端与前端同源，避免两处漂移）。
func PositionVerdictLabel(v string) string {
	switch v {
	case PositionVerdictHold:
		return "继续持有"
	case PositionVerdictTrim:
		return "减仓"
	case PositionVerdictExit:
		return "清仓"
	}
	return v
}

// positionAdviceSystemPrompt 系统提示：封闭枚举 + 只判断不算术 + 引用数值。
const positionAdviceSystemPrompt = `你是一名持仓风控顾问。用户已经持有下列 A 股，请**逐笔**给出卖出决策建议。

你的任务不是分析这些公司值不值得买，而是回答「**这一笔持仓，现在该怎么处理**」。

对每一笔持仓给出封闭枚举结论之一：
- hold：继续持有（当前证据不支持减仓，理由必须具体，不能是「暂无明显风险」）
- trim：减仓（兑现部分利润 / 降低单一敞口 / 风险信号出现但未致命）
- exit：清仓（持有逻辑已破坏，或风险信号足以否决继续持有）

铁律：
1. **所有数值已由系统算好**（成本 cost、现价 price、浮动盈亏 pnl_pct/pnl_amount、持有交易日 held_days、
   持仓期最高价 peak、自峰值回撤 peak_drawdown_pct、仓位占比 weight_pct）——**直接引用，不要自己重算**，
   系统会程序化核对你引用的数字，与数据不符的会被标记展示给用户。
2. 结论必须建立在给出的数据上：触发的提醒信号 signals 与待复核利空事件 events 是最重要的输入，
   命中时必须在理由中点名并说明它对**这笔成本**的含义。
3. **禁止使用你记忆中关于这些公司的信息**，不得虚构财务、新闻、公告、股东行为。数据没给的就说没有依据。
4. invalidation 写「什么情况下这个结论不再成立」（具体价位 / 事件 / 时间窗口），不要写空泛的「市场变化时」。
5. 这是研究参考，不构成投资建议；不要给出加仓建议——本任务只回答持有 / 减仓 / 清仓。

只输出 JSON：{"advices":[{"position_id":123,"symbol":"...","verdict":"hold|trim|exit","reason":"...","invalidation":"..."}]}。
position_id 与 symbol 必须逐字复制对应输入行；同一 symbol 可能有多笔不同成本的持仓，必须按 position_id 分别覆盖，
不要合并或省略。不要任何解释或代码块标记。`

// AdviseAsync 建后台任务（HTTP 秒回）。校验在同步段完成，模型调用在独立 context 上跑。
func (s *PositionAdviceService) AdviseAsync(userID int64, allowPrivate bool, req PositionAdviceRequest) (*LLMTaskView, error) {
	if s.position == nil || s.llm == nil {
		return nil, errors.New("持仓建议服务不可用")
	}
	req.Symbol = strings.TrimSpace(req.Symbol)
	s.registerDurableJobHandler()
	return StartDurableLLMTask(userID, JobKindPositionAdvice, req, allowPrivate)
}

func (s *PositionAdviceService) registerDurableJobHandler() {
	RegisterDurableLLMJobHandler(JobKindPositionAdvice, positionAdviceJobTimeout,
		func(ctx context.Context, userID int64, allowPrivate bool, raw json.RawMessage) (DurableJobResult, error) {
			var req PositionAdviceRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return DurableJobResult{}, fmt.Errorf("持仓建议作业快照无效: %w", err)
			}
			result, err := s.Advise(ctx, userID, allowPrivate, req)
			if err != nil {
				return DurableJobResult{}, err
			}
			return DurableJobResult{
				Value: result, Status: model.JobStatusSuccess, TraceID: result.TraceID,
				Provider: result.Provider, Model: result.Model,
				Total: result.Analyzed, Succeeded: len(result.Advices),
				Failed: max(0, result.Analyzed-len(result.Advices)),
			}, nil
		})
}

// positionAdviceRow 喂给模型的单笔持仓（**全部数值由 Go 算好**）。
type positionAdviceRow struct {
	PositionID int64   `json:"position_id"` // 逐持仓稳定身份，模型必须原样回传
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`       // short_term / long_term
	Cost       float64 `json:"cost"`       // 我的加权成本（元/股）
	Quantity   float64 `json:"quantity"`   // 持有股数
	Price      float64 `json:"price"`      // 当前有效现价
	PnlPct     float64 `json:"pnl_pct"`    // 浮动盈亏 %
	PnlAmount  float64 `json:"pnl_amount"` // 浮动盈亏金额（元）
	HeldDays   int     `json:"held_days"`  // 已持有交易日
	WeightPct  float64 `json:"weight_pct"` // 占我全部已定价持仓市值的比例 %

	BuyReason string `json:"buy_reason,omitempty"` // 我当初为什么买（原始录入）

	Peak            float64 `json:"peak,omitempty"` // 持仓期最高价
	PeakDate        string  `json:"peak_date,omitempty"`
	PeakDrawdownPct float64 `json:"peak_drawdown_pct,omitempty"` // 自峰值回撤 %
	PeakNote        string  `json:"peak_note,omitempty"`         // 前复权回填口径声明

	Signals []string `json:"signals,omitempty"` // D14/D15 命中的提醒
	Events  []string `json:"events,omitempty"`  // D16 待复核的利空事件

}

// Advise 生成逐笔卖出建议（后台任务体；也可被测试直接调用）。
func (s *PositionAdviceService) Advise(ctx context.Context, userID int64, allowPrivate bool, req PositionAdviceRequest) (*PositionAdviceResult, error) {
	views, err := s.position.List(ctx, userID, model.PositionStatusHolding)
	if err != nil {
		return nil, err
	}
	res := &PositionAdviceResult{Advices: []PositionAdvice{}, Notes: []string{}, PromptVer: positionAdvicePromptVersion}
	if len(views) == 0 {
		return nil, errors.New("暂无持仓，请先记录持仓后再获取卖出建议")
	}

	// 已定价市值合计（仓位占比的分母）——**只算取到 fresh 行情的仓**，
	// 与 Overview 的 pricedCost 同思路：分子分母不能一个含旧价一个不含。
	var pricedValue float64
	for _, v := range views {
		if v.QuoteOK {
			pricedValue += v.MarketValue
		}
	}

	matched := 0
	rows := make([]positionAdviceRow, 0, len(views))
	for _, v := range views {
		if req.Symbol != "" && v.Symbol != req.Symbol {
			continue
		}
		matched++
		if !v.QuoteOK {
			// fail-closed：没有当前有效行情就不给这一笔出结论（旧价上的割/守/补有害）。
			res.Skipped++
			continue
		}
		row := positionAdviceRow{
			Symbol: v.Symbol, Name: orSymbol(v.Name, v.Symbol), Type: v.PositionType,
			Cost: round2(v.BuyPrice), Quantity: v.Quantity, Price: round2(v.CurrentPrice),
			PnlPct: round2(v.ProfitPct), PnlAmount: round2(v.ProfitAmount),
			HeldDays: v.HeldTradeDays, BuyReason: truncateRunes(v.BuyReason, 200),
			PositionID: v.ID,
		}
		if pricedValue > 0 {
			row.WeightPct = round2(v.MarketValue / pricedValue * 100)
		}
		if v.Peak != nil {
			row.Peak = v.Peak.Price
			row.PeakDate = v.Peak.Date
			row.PeakDrawdownPct = v.Peak.DrawdownPct
			row.PeakNote = v.Peak.Note
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		if matched == 0 {
			return nil, errors.New("未找到该标的的持仓")
		}
		// 匹配到的持仓全都没有当前有效行情：整体拒答（机读码供前端分支）。
		return nil, refusalErrf(RefusalFreshQuotesInsufficient,
			"%d 笔持仓均无当前有效行情（可能停牌或数据源故障），无法基于旧价给出卖出建议", res.Skipped)
	}
	// 按「浮亏最深」排序后截断：真要决策的是亏得最多的那些，不是列表里排在前面的。
	sortAdviceRowsByUrgency(rows)
	if len(rows) > positionAdviceMaxPositions {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"持仓 %d 笔超过单次分析上限 %d，已按浮亏由深到浅取前 %d 笔，其余本次未分析",
			len(rows), positionAdviceMaxPositions, positionAdviceMaxPositions))
		rows = rows[:positionAdviceMaxPositions]
	}

	// D14/D15 命中信号 + D16 待复核事件（**按持仓行归并**，不是按标的——
	// 同一标的的两笔仓位成本不同，命中的信号也不同）。
	attachAdviceSignals(userID, rows)

	res.Analyzed = len(rows)
	if res.Skipped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%d 笔持仓因无当前有效行情未参与分析（不用旧价出结论）", res.Skipped))
	}

	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, refusalErr(RefusalLLMUnavailable, "AI 建议不可用："+err.Error())
	}
	allowPrivate = llmAllowPrivate(allowPrivate, cfg)
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	convo := []chatMessage{
		{Role: "system", Content: positionAdviceSystemPrompt},
		{Role: "user", Content: "我的持仓与系统算好的数据如下（JSON 数组）：\n" + string(inputJSON)},
	}
	run := newLLMRun(newLLMTraceID(), "", "position_advice", "position_advice.v2", positionAdvicePromptVersion)
	run.hashData(string(inputJSON))
	run.hashPrompt(convo)

	posByID := map[int64]positionAdviceRow{}
	for _, r := range rows {
		posByID[r.PositionID] = r
	}

	type adviceOut struct {
		Advices []PositionAdvice `json:"advices"`
	}
	var lastErr error
	repairLimit := moduleRepairAttempts("position_advice")
	requestMax := moduleTokenCap("position_advice", cfg.MaxTokens)
	for attempt := 0; attempt <= repairLimit; attempt++ {
		result, cerr := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: requestMax,
			Messages: convo, JSONMode: true, AllowPrivate: allowPrivate,
			Repair: attempt > 0, // repair 轮：契约开启时温度固定 0
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(result, cerr)
		if result != nil && result.Usage.TotalTokens > 0 {
			// 一次建议 = 一次手动动作（repair 轮不重复计次，与既有模块同纪律）。
			consumeQuota(userID, result.Usage.TotalTokens, attempt == 0)
		}
		if cerr != nil {
			if attempt < repairLimit && isTokenLimitFinishState(run.FinishState) {
				lastErr = cerr
				requestMax = moduleRepairTokenCap("position_advice", requestMax)
				convo = appendModuleRepairMessages(convo, "position_advice", chatResultContent(result), run.FinishState,
					"上一条输出因 token 上限被截断。请从头完整输出 advices JSON，position_id 与 symbol 必须来自同一输入行。")
				continue
			}
			code := RefusalCodeOf(cerr)
			if code == "" {
				code = RefusalLLMCallFailed
			}
			return nil, refusalErr(code, "AI 卖出建议生成失败："+cerr.Error())
		}
		var out adviceOut
		if jerr := json.Unmarshal([]byte(extractJSONObject(result.Content)), &out); jerr == nil {
			valid := filterPositionAdvices(out.Advices, posByID)
			if len(valid) > 0 {
				run.acceptRouteAttribution()
				res.Advices = valid
				res.LLMConfigID, res.Provider, res.Model = cfg.ID, cfg.Provider, cfg.Model
				res.TraceID = run.TraceID
				res.EvidenceCheck = verifyPositionAdvice(valid, rows)
				if n := len(rows) - len(valid); n > 0 {
					res.Notes = append(res.Notes, fmt.Sprintf(
						"%d 笔持仓模型未给出合法结论（枚举越界或 position_id/symbol 不匹配），已丢弃——未擅自代填「继续持有」", n))
				}
				stampPositionAdviceResult(res, time.Now())
				return res, nil
			}
			lastErr = errors.New("模型未给出任何合法结论")
		} else {
			lastErr = jerr
		}
		convo = appendModuleRepairMessages(convo, "position_advice", result.Content, run.FinishState,
			"上一条输出不合格。请只输出 JSON：{\"advices\":[{\"position_id\",\"symbol\",\"verdict\":\"hold|trim|exit\",\"reason\",\"invalidation\"}]}。position_id 与 symbol 必须来自同一输入行；同代码多仓也要逐笔输出。")
	}
	run.DegradedReason = "llm_output_invalid"
	return nil, refusalErrf(RefusalLLMOutputInvalid, "AI 卖出建议输出不合格：%v", lastErr)
}

// filterPositionAdvices 服务端强校验：枚举越界、ID/代码不匹配、重复 ID、空理由一律丢弃。
// **不代填默认值**——模型没给出合法结论就是没给，代填 hold 会把「没结论」伪装成「建议持有」。
func filterPositionAdvices(in []PositionAdvice, positions map[int64]positionAdviceRow) []PositionAdvice {
	out := make([]PositionAdvice, 0, len(in))
	seen := map[int64]bool{}
	for _, a := range in {
		a.Symbol = strings.TrimSpace(a.Symbol)
		a.Verdict = normalizePositionVerdict(a.Verdict)
		a.Reason = truncateRunes(strings.TrimSpace(a.Reason), 400)
		a.Invalidation = truncateRunes(strings.TrimSpace(a.Invalidation), 300)
		p, ok := positions[a.PositionID]
		if !ok || p.Symbol != a.Symbol || seen[a.PositionID] || a.Verdict == "" ||
			a.Reason == "" || a.Invalidation == "" {
			continue
		}
		seen[a.PositionID] = true
		// 名称、持仓类型、成本和数量一律由服务端快照回填，不能信任模型自报。
		a.Name = p.Name
		a.PositionType = p.Type
		a.Cost = p.Cost
		a.Quantity = p.Quantity
		out = append(out, a)
	}
	return out
}

// verifyPositionAdvice 逐仓核验模型引用的数字。每条建议只能使用其 position_id 对应行的
// 成本/现价/盈亏/持有天数/峰值/回撤/仓位占比，不能拿另一笔仓位的数字串线佐证。
func verifyPositionAdvice(advices []PositionAdvice, rows []positionAdviceRow) *evidenceCheck {
	rowByID := make(map[int64]positionAdviceRow, len(rows))
	for _, row := range rows {
		rowByID[row.PositionID] = row
	}
	check := &evidenceCheck{
		Version:    "ev5",
		KeySection: &evidenceKeySection{Module: "建议"},
	}
	evidenceSeq := 0
	for _, a := range advices {
		row, ok := rowByID[a.PositionID]
		if !ok || row.Symbol != a.Symbol {
			continue
		}
		label := orSymbol(a.Name, a.Symbol)
		sections := []evidenceSection{{Module: "建议", Text: a.Reason}}
		if a.Invalidation != "" {
			sections = append(sections, evidenceSection{Module: "失效条件", Text: a.Invalidation})
		}
		one := verifyEvidenceLabeled(sections, snapshotLabeledValues(row, nil))
		markKeySection(one, "建议")
		check.Total += one.Total
		check.Matched += one.Matched
		check.SkippedCount += one.SkippedCount
		check.UnmatchedTotal += one.UnmatchedTotal
		check.SnapshotMatched += one.SnapshotMatched
		check.PlanMatched += one.PlanMatched
		check.UserMatched += one.UserMatched
		check.ContextMatched += one.ContextMatched
		check.Truncated = check.Truncated || one.Truncated
		if one.KeySection != nil {
			check.KeySection.Total += one.KeySection.Total
			check.KeySection.SnapshotMatched += one.KeySection.SnapshotMatched
		}
		for _, raw := range one.Unmatched {
			if len(check.Unmatched) >= 10 {
				break
			}
			check.Unmatched = append(check.Unmatched, raw)
		}
		for _, item := range one.Items {
			item.Module = fmt.Sprintf("%s#%d·%s", label, a.PositionID, item.Module)
			if item.Matched {
				evidenceSeq++
				item.EvidenceID = fmt.Sprintf("ev-%03d", evidenceSeq)
			}
			if len(check.Items) < 50 {
				check.Items = append(check.Items, item)
			} else {
				check.Truncated = true
			}
		}
	}
	return check
}

// sortAdviceRowsByUrgency 按紧急度排序：浮亏最深的在前，其次自峰值回撤最深的。
// 稳定口径（同分按代码升序）保证同一批持仓每次排序结果一致、截断边界可复现。
func sortAdviceRowsByUrgency(rows []positionAdviceRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.PnlPct != b.PnlPct {
			return a.PnlPct < b.PnlPct
		}
		if a.PeakDrawdownPct != b.PeakDrawdownPct {
			return a.PeakDrawdownPct > b.PeakDrawdownPct
		}
		return a.Symbol < b.Symbol
	})
}

// attachAdviceSignals 给每一笔持仓挂上 D14/D15 命中的提醒与 D16 待复核事件。
// **按 position_id 归并**——同一标的的两笔仓位成本不同，触发的信号本就不同。
// 读取失败只是「本次没有信号段」，不阻断建议生成（信号是加分项不是前置条件）。
func attachAdviceSignals(userID int64, rows []positionAdviceRow) {
	if common.DB == nil || len(rows) == 0 {
		return
	}
	posIDs := make([]int64, 0, len(rows))
	idx := make(map[int64]int, len(rows))
	for i, r := range rows {
		posIDs = append(posIDs, r.PositionID)
		idx[r.PositionID] = i
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	// D14/D15：今日未读的持仓类提醒命中（带 position_id）。
	var events []model.AlertEvent
	if err := common.DB.Where("user_id = ? AND position_id IN ? AND trade_date = ? AND status = ?",
		userID, posIDs, today, model.AlertEventUnread).Find(&events).Error; err == nil {
		for _, e := range events {
			if i, ok := idx[e.PositionID]; ok {
				rows[i].Signals = append(rows[i].Signals, e.Message)
			}
		}
	} else {
		common.SysWarn("持仓建议读取提醒信号失败 user=%d: %v", userID, err)
	}
	// D16：未处理的卖出复核。
	var reviews []model.SellReview
	if err := common.DB.Where("user_id = ? AND position_id IN ? AND status = ?",
		userID, posIDs, model.SellReviewStatusOpen).Order("trade_date DESC").Find(&reviews).Error; err == nil {
		for _, r := range reviews {
			if i, ok := idx[r.PositionID]; ok {
				rows[i].Events = append(rows[i].Events,
					fmt.Sprintf("[%s] %s：%s", r.TradeDate, r.Title, r.Detail))
			}
		}
	} else {
		common.SysWarn("持仓建议读取卖出复核失败 user=%d: %v", userID, err)
	}
}
