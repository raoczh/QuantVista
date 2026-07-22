package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"quantvista/model"
)

// P0-2 统一运行元数据 + P0-8 调用关联/完整性元数据（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §5.1/§7.1）。
//
// 关联语义（不可回退的契约）：
//   - trace_id：一个业务结果（一次分析记录/一个推荐批次/一份日报/一个问答会话/一次对比等）
//     对应一个 trace_id；该业务运行内的主调、repair、复核、反方、交易计划、确定性降级共享它。
//   - run_id：一个「逻辑调用组」对应一个 run_id——主调与它的全部 repair 轮共享同一 run_id
//     （靠 attempt 区分轮次）；复核/反方/交易计划各是独立 run。
//   - parent_run_id：派生调用（复核/反方/交易计划）回指主调 run_id；主调为空。
//   - attempt：1 基（1=首轮，>1=repair 轮）；repair = attempt > 1；
//     聚合层恒满足 attempt_count = 1 + repair_count。
//   - 业务表落 trace_id（+ llm_run_json manifest），llm_call_logs 落 trace/run/parent/attempt，
//     两侧凭 trace_id 双向关联；旧记录这些字段为空，读取兼容。
//   - 正文策略不变：审计正文原样保留、仅 60KB 截断，本层只补关联与完整性元数据（P0-8）。

// newLLMTraceID / newLLMRunID 生成 128bit 随机 hex ID。禁止改成时间戳自增等可预测形态
// （trace_id 会透出到前端响应，可枚举 ID 有越权探测面）。
func newLLMTraceID() string { return "t" + randomHexID() }
func newLLMRunID() string   { return "r" + randomHexID() }

func randomHexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见（系统熵源故障）：退化为空串，调用链照常工作只是缺关联 ID，
		// 不能因审计元数据把业务调用打挂。
		return ""
	}
	return hex.EncodeToString(b[:])
}

// llmContentHash 稳定内容 hash（sha256 hex，带算法前缀便于将来换算法归因）。
func llmContentHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// llmPromptHash 对初始消息序列（进入首轮请求前的 system+user 形态）计算 hash。
// repair 轮追加的 assistant/user 消息不改变本值——prompt_hash 标识「这组 prompt」，
// 轮次差异由 attempt 表达。注意这是 ac1 注入前的业务消息形态；契约版本由
// llm_contract.go 的 llmAccuracyContractVersion 单独归因。
func llmPromptHash(messages []chatMessage) string {
	b, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	return llmContentHash(string(b))
}

// llmRun 一个逻辑调用组（主调+其 repair 轮）的运行上下文：模块层创建并贯穿 repair 循环，
// 结束后由 manifest() 输出 §5.1 契约（业务表 llm_run_json）。
type llmRun struct {
	TraceID     string
	RunID       string
	ParentRunID string
	Module      string // 与 llm_call_logs.module 同名同值（business_result_type 粒度）

	SchemaVersion string // 输出契约版本（如 analysis.v1；自由文本模块 *.free_text.v1）
	PromptVersion string // 业务 prompt 版本（p19/d4/q12/sp3/-custom 等，与既有版本号同源）
	PromptHash    string
	DataHash      string // 输入数据快照 hash（喂给模型的数据 JSON）

	Attempts       int    // 实际发出的上游请求次数（1 基计数的最大 attempt）
	FinishState    string // 最后一次请求的规范化终态（normalizeLLMFinishState）
	FinishStateRaw string // 上游原始 finish_reason/status
	DegradedReason string // 业务层降级原因（quant_fallback/llm_output_invalid 等；空=未降级）
	// structuredDropped 本 run 内 JSON mode 是否发生过回落（P0-2 修复批）：中央客户端在
	// 声明化路由或隐式回落点经 chatMeta.StructuredDropped 指针置位。manifest 据此输出
	// 「最终实际生效」的 structured_method——同一 run 的主调与 repair 轮同配置同目标，
	// 回落具备粘性（能力观察写入后声明化路由接管），任一 attempt 回落即整个 run 按
	// free_text 归因（多 attempt 汇总语义，见 §5.1）。
	structuredDropped bool
}

// newLLMRun 创建调用组。parentRunID 为空表示主调。
func newLLMRun(traceID, parentRunID, module, schemaVersion, promptVersion string) *llmRun {
	return &llmRun{
		TraceID: traceID, RunID: newLLMRunID(), ParentRunID: parentRunID,
		Module: module, SchemaVersion: schemaVersion, PromptVersion: promptVersion,
	}
}

// hashPrompt / hashData 计算并固化输入指纹（每 run 一次，repair 轮不重算）。
func (r *llmRun) hashPrompt(messages []chatMessage) { r.PromptHash = llmPromptHash(messages) }
func (r *llmRun) hashData(data string)              { r.DataHash = llmContentHash(data) }

// chatMeta 为第 attempt 轮（1 基）构造审计元数据，并推进本 run 的 attempt 计数。
// 各模块 repair 循环内以 attempt+1 调用（loop 变量 0 基）。
func (r *llmRun) chatMeta(userID int64, cfg *model.LLMConfig, attempt int) chatMeta {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > r.Attempts {
		r.Attempts = attempt
	}
	m := chatMeta{
		CallerUserID: userID,
		TraceID:      r.TraceID, RunID: r.RunID, ParentRunID: r.ParentRunID,
		Module: r.Module, Attempt: attempt,
		SchemaVersion: r.SchemaVersion, PromptVersion: r.PromptVersion,
		PromptHash: r.PromptHash, DataHash: r.DataHash,
		StructuredDropped: &r.structuredDropped, // 中央客户端回落时置位，manifest 消费
	}
	if cfg != nil {
		m.ConfigID = cfg.ID
		m.Provider = cfg.Provider
	}
	return m
}

// record 记录本 run 最后一次请求的终态（成功与失败都要记；err 含 RefusalError 时
// 归一到规范化枚举）。P0-8 修复批起完整性拒收路径的 res 也带真实原始终态（audit
// outcome），raw 优先取上游真值；res 为 nil（网络失败等无响应场景）时按 err 归类。
func (r *llmRun) record(res *chatResult, err error) {
	raw := ""
	if res != nil {
		raw = res.FinishReason
	}
	r.FinishStateRaw = raw
	r.FinishState = normalizeLLMFinishState(raw, err)
}

// LLMRunManifest §5.1 目标契约的本批实现子集（业务表 llm_run_json 数组元素）。
// coverage 字段本批仅预留（P1-1 起由推荐 coverage 诊断填充），恒省略。
type LLMRunManifest struct {
	RunID       string `json:"run_id"`
	TraceID     string `json:"trace_id"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	Module      string `json:"module"`

	SchemaVersion string `json:"schema_version,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
	PromptHash    string `json:"prompt_hash,omitempty"`
	DataHash      string `json:"data_snapshot_hash,omitempty"`
	// StructuredMethod 本 run 最终实际生效的结构化方法（P0-2 修复批）：JSON mode 因端点
	// 不支持而回落（声明化路由或运行时 fallback）时如实记 free_text，不再写入口意图。
	// 多 attempt 汇总语义：任一 attempt 回落即整个 run 记 free_text（同 run 各 attempt 同
	// 配置同目标，回落有粘性）；逐请求真值仍以 llm_call_logs.structured_method 为准。
	StructuredMethod string `json:"structured_method,omitempty"`

	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	LLMConfigID  int64  `json:"llm_config_id,omitempty"`
	EndpointType string `json:"endpoint_type,omitempty"`

	AttemptCount int `json:"attempt_count"` // = 1 + RepairCount（不变式，manifest() 保证）
	RepairCount  int `json:"repair_count"`  // 首轮之后的额外次数
	// OutputBudget 模块声明的输出 token 预算（P0-9，llm_budget.go；实际请求值为
	// min(用户配置, 本值)，见 moduleTokenCap）。0=未声明（省略）。
	OutputBudget int `json:"output_budget,omitempty"`

	FinishState    string `json:"finish_state,omitempty"`
	FinishStateRaw string `json:"finish_state_raw,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// manifest 输出本 run 的运行元数据。jsonMode 为该模块的请求口径；最终实际生效形态在
// run 内发生过回落（structuredDropped）时如实按 free_text 归因。
func (r *llmRun) manifest(cfg *model.LLMConfig, jsonMode bool) LLMRunManifest {
	m := LLMRunManifest{
		RunID: r.RunID, TraceID: r.TraceID, ParentRunID: r.ParentRunID, Module: r.Module,
		SchemaVersion: r.SchemaVersion, PromptVersion: r.PromptVersion,
		PromptHash: r.PromptHash, DataHash: r.DataHash,
		StructuredMethod: structuredMethodName(jsonMode && !r.structuredDropped),
		AttemptCount:     r.Attempts,
		OutputBudget:     moduleBudget(r.Module).MaxTokens,
		FinishState:      r.FinishState, FinishStateRaw: r.FinishStateRaw,
		DegradedReason: r.DegradedReason,
	}
	if m.AttemptCount < 1 {
		m.AttemptCount = 1 // 防御：manifest 只在实际发起过调用的 run 上输出
	}
	m.RepairCount = m.AttemptCount - 1
	if cfg != nil {
		m.Provider = cfg.Provider
		m.Model = cfg.Model
		m.LLMConfigID = cfg.ID
		m.EndpointType = cfg.EndpointType
	}
	return m
}

// marshalLLMRunManifests 把实际发起过调用（Attempts>0）的 run 序列化为 manifest 数组 JSON。
// 全部未发起（如确定性拒绝零 token 路径）返回空串，业务列保持空。
func marshalLLMRunManifests(cfg *model.LLMConfig, entries ...llmRunManifestEntry) string {
	out := make([]LLMRunManifest, 0, len(entries))
	for _, e := range entries {
		if e.run == nil || e.run.Attempts == 0 {
			continue
		}
		out = append(out, e.run.manifest(cfg, e.jsonMode))
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// llmRunManifestEntry marshalLLMRunManifests 的入参：run + 该模块请求口径。
type llmRunManifestEntry struct {
	run      *llmRun
	jsonMode bool
}

func runEntry(run *llmRun, jsonMode bool) llmRunManifestEntry {
	return llmRunManifestEntry{run: run, jsonMode: jsonMode}
}

// structuredMethodName 结构化方法枚举（本项目仅两态；json_schema/function_calling 属
// P0-5 capability matrix 之后的能力）。
func structuredMethodName(jsonMode bool) string {
	if jsonMode {
		return model.LLMStructuredJSONObject
	}
	return model.LLMStructuredFreeText
}

// normalizeLLMFinishState 把上游原始终态 + 调用错误归一为规范化枚举（§5.1）：
// stop | tool_calls | completed | length | max_tokens | content_filter | failed |
// cancelled | eof_without_marker | error | unknown | ""（成功但上游未报告，兼容网关）。
// 原始值另存 finish_state_raw，本枚举不得夹带 provider 私有词表。
func normalizeLLMFinishState(raw string, callErr error) string {
	n := strings.ToLower(strings.TrimSpace(raw))
	if callErr == nil {
		switch n {
		case "stop", "tool_calls", "completed",
			// 关闭契约的旧兼容路径可能带异常终态仍算成功：如实记录，不粉饰成 stop。
			"length", "max_tokens", "content_filter", "failed", "cancelled":
			return n
		case "":
			return "" // 成功但上游未报告 finish_reason（兼容网关整包形态）
		default:
			return "unknown"
		}
	}
	switch RefusalCodeOf(callErr) {
	case RefusalLLMContentFiltered:
		return "content_filter"
	case RefusalLLMResponseIncomplete:
		switch n {
		case "length", "max_tokens":
			return n
		}
		msg := callErr.Error()
		// 错误路径下 res 常为 nil（原始终态只存在于错误文案里）：从门禁文案还原截断语义，
		// 避免 length/max_tokens 被笼统归为 error。判序：具体枚举 → EOF → 截断提示。
		if strings.Contains(msg, "finish_reason=length") {
			return "length"
		}
		if strings.Contains(msg, "finish_reason=max_tokens") || strings.Contains(msg, "max_output_tokens") {
			return "max_tokens"
		}
		if strings.Contains(msg, "eof_without_marker") || strings.Contains(msg, "未收到终止标记") {
			return "eof_without_marker"
		}
		return "error"
	case RefusalLLMCallFailed:
		switch n {
		case "failed", "cancelled":
			return n
		}
		return "error"
	default:
		// 其余错误（网络/HTTP/配置等，或未附码的旧路径错误）。
		return "error"
	}
}
