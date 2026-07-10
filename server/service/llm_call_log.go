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
	llmCallBodyLimit = 60 << 10
	llmCallRetention = 90 * 24 * time.Hour
)

type chatMeta struct {
	CallerUserID int64
	Module       string
	ConfigID     int64
	Provider     string
}

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
	if res != nil {
		responseBody = res.Content
		usage = res.Usage
		if res.LatencyMs > 0 {
			latencyMs = res.LatencyMs
		}
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
		RequestBody:      truncateAuditText(string(requestJSON), llmCallBodyLimit),
		ResponseBody:     truncateAuditText(responseBody, llmCallBodyLimit),
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

func (s *AdminService) ListLLMCalls(userID int64, module, status string, page, pageSize int) (*LLMCallLogList, error) {
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
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	var logs []model.LLMCallLog
	if err := q.Select("id,user_id,module,llm_config_id,provider,model,endpoint_type,stream,status,error_msg,prompt_tokens,completion_tokens,total_tokens,latency_ms,created_at").
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
