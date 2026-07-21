package service

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"quantvista/common"
	"quantvista/model"
)

const (
	// llmCallBodyLimit 单字段正文上限：仅做长度截断防库膨胀，**不做内容脱敏**。
	// 管理端 LLM 调用审计面向管理员排障，请求/响应原文（含用户问题、持仓上下文）
	// 原样保留；API key 本就不进 messages（走 Authorization 头），勿把「截断」当「脱敏」。
	// 计划文档 P0-8 的「审计脱敏」项已按产品决策取消（见 LLM_ACCURACY_OPTIMIZATION_PLAN）。
	llmCallBodyLimit = 60 << 10
	llmCallRetention = 90 * 24 * time.Hour
)

type chatMeta struct {
	CallerUserID int64
	Module       string
	ConfigID     int64
	Provider     string

	// ---- P0-2 调用关联元数据（llm_run.go 的 llmRun.chatMeta 统一构造；直填仅限探针/测试）----
	TraceID       string
	RunID         string
	ParentRunID   string
	Attempt       int // 1 基；0=未接线路径未记录
	SchemaVersion string
	PromptVersion string
	PromptHash    string
	DataHash      string
}

// writeLLMCallLog 落一条调用审计。正文为管理员排障用原文：messages + 响应 content 仅
// truncateAuditText 限长，不做敏感字段打码/哈希替换（与 P0-8 取消脱敏的产品决策一致）。
func writeLLMCallLog(p chatParams, stream bool, res *chatResult, callErr error, elapsed time.Duration) {
	if common.DB == nil {
		return
	}
	requestJSON, marshalErr := json.Marshal(p.Messages)
	if marshalErr != nil {
		requestJSON = []byte("[]")
	}

	status := model.LLMCallStatusSuccess
	errorMsg := ""
	responseBody := ""
	usage := chatUsage{}
	latencyMs := elapsed.Milliseconds()
	firstChunkMs := int64(0)
	if res != nil {
		responseBody = res.Content
		usage = res.Usage
		if res.LatencyMs > 0 {
			latencyMs = res.LatencyMs
		}
		firstChunkMs = res.FirstChunkMs
	}
	if callErr != nil {
		status = model.LLMCallStatusError
		errorMsg = truncateAuditText(callErr.Error(), 512)
		responseBody = callErr.Error()
	}

	endpointType := p.EndpointType
	if endpointType == "" {
		endpointType = model.LLMEndpointChat
	}
	// 实际生效的结构化方法：JSON mode 因端点不支持而回落时内部会把 effectiveJSONMode
	// 置 false（chat/responses、流式/非流式四处回落点），审计记录真实形态。
	effectiveJSON := p.JSONMode
	if p.effectiveJSONMode != nil {
		effectiveJSON = *p.effectiveJSONMode
	}
	finishRaw := ""
	if res != nil {
		finishRaw = res.FinishReason
	}
	row := &model.LLMCallLog{
		UserID:           p.Meta.CallerUserID,
		Module:           p.Meta.Module,
		LLMConfigID:      p.Meta.ConfigID,
		Provider:         p.Meta.Provider,
		Model:            p.Model,
		EndpointType:     endpointType,
		Stream:           stream,
		Status:           status,
		ErrorMsg:         errorMsg,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		LatencyMs:        latencyMs,
		FirstChunkMs:     firstChunkMs,
		RequestBody:      truncateAuditText(string(requestJSON), llmCallBodyLimit),
		ResponseBody:     truncateAuditText(responseBody, llmCallBodyLimit),

		// P0-2/P0-8 关联与完整性元数据（旧记录为空，读取兼容）。
		TraceID:          p.Meta.TraceID,
		RunID:            p.Meta.RunID,
		ParentRunID:      p.Meta.ParentRunID,
		Attempt:          p.Meta.Attempt,
		Repair:           p.Repair,
		StructuredMethod: structuredMethodName(effectiveJSON),
		SchemaVersion:    p.Meta.SchemaVersion,
		PromptVersion:    p.Meta.PromptVersion,
		PromptHash:       p.Meta.PromptHash,
		DataHash:         p.Meta.DataHash,
		FinishState:      normalizeLLMFinishState(finishRaw, callErr),
		FinishStateRaw:   finishRaw,
	}
	if err := common.DB.Create(row).Error; err != nil {
		common.SysWarn("LLM 调用审计写入失败(module=%s user=%d): %v", p.Meta.Module, p.Meta.CallerUserID, err)
	}
}

func truncateAuditText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	const suffix = "\n...[truncated]"
	keep := limit - len(suffix)
	if keep <= 0 {
		return suffix[:limit]
	}
	b := []byte(s)[:keep]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + suffix
}

type LLMCallLogView struct {
	model.LLMCallLog
	Username string `json:"username"`
}

type LLMCallLogList struct {
	Items []LLMCallLogView `json:"items"`
	Total int64            `json:"total"`
}

func (s *AdminService) ListLLMCalls(userID int64, module, status, trace string, page, pageSize int) (*LLMCallLogList, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	q := common.DB.Model(&model.LLMCallLog{})
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	if module = strings.TrimSpace(module); module != "" {
		q = q.Where("module = ?", module)
	}
	if status = strings.TrimSpace(status); status != "" {
		q = q.Where("status = ?", status)
	}
	// P0-2 追溯筛选：按业务结果的 trace_id（或某个 run_id）列出其全部关联调用
	//（主调/repair/复核/反方/交易计划一屏可见）。
	if trace = strings.TrimSpace(trace); trace != "" {
		q = q.Where("trace_id = ? OR run_id = ?", trace, trace)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	var logs []model.LLMCallLog
	if err := q.Select("id,user_id,module,llm_config_id,provider,model,endpoint_type,stream,status,error_msg,prompt_tokens,completion_tokens,total_tokens,latency_ms,first_chunk_ms," +
		"trace_id,run_id,parent_run_id,attempt,repair,structured_method,schema_version,prompt_version,prompt_hash,data_hash,finish_state,finish_state_raw,created_at").
		Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&logs).Error; err != nil {
		return nil, err
	}
	return &LLMCallLogList{Items: llmCallViews(logs), Total: total}, nil
}

func (s *AdminService) GetLLMCall(id int64) (*LLMCallLogView, error) {
	var log model.LLMCallLog
	if err := common.DB.First(&log, id).Error; err != nil {
		return nil, errors.New("LLM 调用记录不存在")
	}
	rows := llmCallViews([]model.LLMCallLog{log})
	return &rows[0], nil
}

func llmCallViews(logs []model.LLMCallLog) []LLMCallLogView {
	userIDs := make([]int64, 0, len(logs))
	seen := map[int64]bool{}
	for _, row := range logs {
		if row.UserID > 0 && !seen[row.UserID] {
			seen[row.UserID] = true
			userIDs = append(userIDs, row.UserID)
		}
	}
	names := map[int64]string{}
	if len(userIDs) > 0 {
		var users []model.User
		if err := common.DB.Select("id,username,display_name").Where("id IN ?", userIDs).Find(&users).Error; err == nil {
			for _, user := range users {
				name := strings.TrimSpace(user.DisplayName)
				if name == "" {
					name = user.Username
				}
				names[user.ID] = name
			}
		}
	}
	views := make([]LLMCallLogView, 0, len(logs))
	for _, row := range logs {
		views = append(views, LLMCallLogView{LLMCallLog: row, Username: names[row.UserID]})
	}
	return views
}

func cleanupLLMCallLogsBefore(cutoff time.Time) (int64, error) {
	if common.DB == nil {
		return 0, nil
	}
	result := common.DB.Where("created_at < ?", cutoff).Delete(&model.LLMCallLog{})
	return result.RowsAffected, result.Error
}

func StartLLMLogJobs() {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 3, 25, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			if n, err := cleanupLLMCallLogsBefore(time.Now().Add(-llmCallRetention)); err != nil {
				common.SysWarn("LLM 调用审计清理失败: %v", err)
			} else if n > 0 {
				common.SysLog("LLM 调用审计清理完成: %d 条", n)
			}
		}
	}()
}
