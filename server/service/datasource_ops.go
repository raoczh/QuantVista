package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

const (
	dataSourceProbeTask  = "datasource_probe"
	dataSourceUncoolTask = "datasource_uncool"
	dataSourceReasonMax  = 200
)

// DataSourceProbeRequest 是管理员单能力探测请求；三元组必须命中代码注册表。
type DataSourceProbeRequest struct {
	Provider   string `json:"provider"`
	Capability string `json:"capability"`
	Market     string `json:"market"`
}

// DataSourceUncoolRequest 是管理员人工解冷请求。
type DataSourceUncoolRequest struct {
	Provider   string `json:"provider"`
	Capability string `json:"capability"`
	Market     string `json:"market"`
	Reason     string `json:"reason"`
}

type DataSourceProbeOperation struct {
	Result  datasource.ProbeResult `json:"result"`
	AuditID int64                  `json:"audit_id"`
}

type DataSourceUncoolOperation struct {
	Provider          string `json:"provider"`
	Capability        string `json:"capability"`
	Market            string `json:"market"`
	Cleared           bool   `json:"cleared"`
	CooldownBeforeSec int    `json:"cooldown_before_sec"`
	AuditID           int64  `json:"audit_id"`
}

var sensitiveAuditReason = regexp.MustCompile(`(?i)(authorization|bearer|cookie|api[_ -]?key|token|secret|password|credential|sk-)`)

func registeredDataSourceTuple(provider, capability, market string) (DataSourceProbeRequest, error) {
	spec, ok := datasource.LookupCapability(provider, capability, market)
	if !ok {
		return DataSourceProbeRequest{}, datasource.ErrProbeNotAllowed
	}
	return DataSourceProbeRequest{Provider: spec.Source, Capability: spec.Capability, Market: spec.Market}, nil
}

func validateDataSourceReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("解冷原因不能为空")
	}
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > dataSourceReasonMax {
		return "", fmt.Errorf("解冷原因长度不能超过 %d 个字符", dataSourceReasonMax)
	}
	for _, r := range reason {
		if r == '\r' || r == '\n' || r == '\x00' || r == '\t' {
			return "", errors.New("解冷原因包含非法控制字符")
		}
	}
	if sensitiveAuditReason.MatchString(reason) {
		return "", errors.New("解冷原因不得包含凭据字段")
	}
	return reason, nil
}

type dataSourceAuditSummary struct {
	Provider          string                  `json:"provider"`
	Capability        string                  `json:"capability"`
	Market            string                  `json:"market"`
	SampleCount       int                     `json:"sample_count,omitempty"`
	Outcome           datasource.ProbeOutcome `json:"outcome,omitempty"`
	Code              string                  `json:"code,omitempty"`
	Reason            string                  `json:"reason,omitempty"`
	Cleared           *bool                   `json:"cleared,omitempty"`
	CooldownBeforeSec int                     `json:"cooldown_before_sec,omitempty"`
}

func encodeDataSourceAuditSummary(summary dataSourceAuditSummary) string {
	raw, _ := json.Marshal(summary)
	return string(raw)
}

// beginDataSourceAudit 必须在任何探测/解冷副作用前成功。初始行按中断失败记账，
// 完成后再用白名单摘要收敛，确保不会出现已执行但完全没有审计记录的操作。
func beginDataSourceAudit(task string, userID int64, tuple DataSourceProbeRequest, summary dataSourceAuditSummary) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	log := &model.DataSyncLog{
		Task: task, Market: tuple.Market, Status: "failed", Total: 1, Failed: 1,
		Message: "operation interrupted", TriggerSource: "admin", UserID: userID,
		ParameterSummary: encodeDataSourceAuditSummary(summary),
	}
	if err := common.DB.Create(log).Error; err != nil {
		common.SysWarn("写数据源运维审计失败 task=%s: %v", task, err)
		return nil, errors.New("数据源运维审计写入失败")
	}
	return log, nil
}

func finishDataSourceAudit(log *model.DataSyncLog, status, message string, summary dataSourceAuditSummary, started time.Time) error {
	if log == nil || log.ID <= 0 || common.DB == nil {
		return errors.New("数据源运维审计不可用")
	}
	succeeded, failed := 0, 0
	if status == "success" {
		succeeded = 1
	} else if status == "failed" {
		failed = 1
	}
	res := common.DB.Model(&model.DataSyncLog{}).Where("id = ?", log.ID).Updates(map[string]any{
		"status": status, "succeeded": succeeded, "failed": failed,
		"duration_ms": time.Since(started).Milliseconds(), "message": message,
		"parameter_summary": encodeDataSourceAuditSummary(summary),
	})
	if res.Error != nil || res.RowsAffected != 1 {
		common.SysWarn("更新数据源运维审计失败 id=%d: %v", log.ID, res.Error)
		return errors.New("数据源运维审计更新失败")
	}
	return nil
}

// ProbeDataSource 执行一次管理员主动探测，并无论成功、空响应还是上游错误都写安全审计。
func (s *MarketService) ProbeDataSource(ctx context.Context, userID int64, in DataSourceProbeRequest) (DataSourceProbeOperation, error) {
	if s == nil || s.mgr == nil {
		return DataSourceProbeOperation{}, datasource.ErrProbeUnavailable
	}
	var err error
	in, err = registeredDataSourceTuple(in.Provider, in.Capability, in.Market)
	if err != nil {
		return DataSourceProbeOperation{}, err
	}
	started := time.Now()
	summary := dataSourceAuditSummary{Provider: in.Provider, Capability: in.Capability, Market: in.Market, SampleCount: 1}
	audit, err := beginDataSourceAudit(dataSourceProbeTask, userID, in, summary)
	if err != nil {
		return DataSourceProbeOperation{}, err
	}
	result, probeErr := s.mgr.Probe(ctx, in.Provider, in.Capability, in.Market)
	status := "failed"
	message := "probe failed"
	if probeErr == nil {
		if result.Outcome == datasource.ProbeSuccess {
			status, message = "success", "probe success"
		} else {
			// DataSyncLog 的 partial 表示“请求完成但样本为空”，与健康滑窗
			// 的 empty 三态一一对应；不把空响应误报为上游错误。
			status, message = "partial", "probe empty"
		}
	}
	summary.SampleCount, summary.Outcome, summary.Code = result.SampleCount, result.Outcome, result.Code
	auditErr := finishDataSourceAudit(audit, status, message, summary, started)
	op := DataSourceProbeOperation{Result: result, AuditID: audit.ID}
	if probeErr != nil && auditErr != nil {
		return op, errors.Join(probeErr, auditErr)
	}
	if probeErr != nil {
		return op, probeErr
	}
	return op, auditErr
}

// UncoolDataSource 只解除指定注册三元组的 cooldown，保留其历史窗口和观测记录。
func (s *MarketService) UncoolDataSource(_ context.Context, userID int64, in DataSourceUncoolRequest) (DataSourceUncoolOperation, error) {
	if s == nil || s.mgr == nil {
		return DataSourceUncoolOperation{}, datasource.ErrProbeUnavailable
	}
	reason, err := validateDataSourceReason(in.Reason)
	if err != nil {
		return DataSourceUncoolOperation{}, err
	}
	tuple, err := registeredDataSourceTuple(in.Provider, in.Capability, in.Market)
	if err != nil {
		return DataSourceUncoolOperation{}, err
	}
	started := time.Now()
	summary := dataSourceAuditSummary{Provider: tuple.Provider, Capability: tuple.Capability, Market: tuple.Market, Reason: reason}
	audit, err := beginDataSourceAudit(dataSourceUncoolTask, userID, tuple, summary)
	if err != nil {
		return DataSourceUncoolOperation{}, err
	}
	left, cleared, uncoolErr := s.mgr.ClearCooldown(tuple.Provider, tuple.Capability, tuple.Market)
	status := "failed"
	message := "uncool failed"
	if uncoolErr == nil {
		status, message = "success", "cooldown cleared"
		if !cleared {
			message = "no cooldown to clear"
		}
	}
	summary.Cleared, summary.CooldownBeforeSec = &cleared, left
	auditErr := finishDataSourceAudit(audit, status, message, summary, started)
	op := DataSourceUncoolOperation{Provider: tuple.Provider, Capability: tuple.Capability, Market: tuple.Market,
		Cleared: cleared, CooldownBeforeSec: left, AuditID: audit.ID}
	if uncoolErr != nil && auditErr != nil {
		return op, errors.Join(uncoolErr, auditErr)
	}
	if uncoolErr != nil {
		return op, uncoolErr
	}
	return op, auditErr
}
