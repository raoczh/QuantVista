package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const dataSyncJobSnapshotVersion = 1

// DataSyncJobRequest 只含经过解析的白名单调度参数，绝不保存 HTTP 请求正文。
type DataSyncJobRequest struct {
	Version          int                `json:"version"`
	Task             string             `json:"task"`
	Market           string             `json:"market,omitempty"`
	Maintenance      MaintenanceRequest `json:"maintenance,omitempty"`
	HasMaintenance   bool               `json:"has_maintenance,omitempty"`
	BarLimit         int                `json:"bar_limit,omitempty"`
	TriggerSource    string             `json:"trigger_source"`
	ParameterSummary string             `json:"parameter_summary,omitempty"`
	Reason           string             `json:"reason,omitempty"`
}

var (
	systemScheduleRetryDelay = 5 * time.Second
	systemScheduleMu         sync.Mutex
	systemSchedulePending    = map[string]struct{}{}
)

func RegisterDataSyncJobHandlers(svc *MarketService) {
	if svc == nil {
		return
	}
	binding := dataSyncJobBinding()
	for _, spec := range []struct {
		kind    string
		timeout time.Duration
	}{
		{JobKindSyncDailyBars, 30 * time.Minute},
		{JobKindBackfillCalendar, 2 * time.Minute},
		{JobKindSnapshotMarket, 2 * time.Minute},
		{JobKindSyncMarketWide, 15 * time.Minute},
		{JobKindInitMarketHistory, 3 * time.Hour},
		{JobKindFactorRebuild, 3 * time.Minute},
	} {
		defaultJobRuntime.registerWithBinding(spec.kind, spec.timeout, svc.dataSyncJobHandler, binding, false)
	}
}

func dataSyncJobBinding() durableJobBinding {
	persist := func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, status, code, message string, now time.Time) error {
		if run.ResultID == nil {
			return errors.New("数据同步作业缺少结果引用")
		}
		log, ok := result.Value.(*model.DataSyncLog)
		if !ok || log == nil {
			return errors.New("数据同步作业缺少结果日志")
		}
		logStatus := log.Status
		if status == model.JobStatusCanceled {
			logStatus = model.JobStatusCanceled
		} else if status == model.JobStatusFailed {
			logStatus = model.JobStatusFailed
		}
		if strings.TrimSpace(log.Message) == "" {
			log.Message = sanitizeJobError(message)
		}
		updates := map[string]any{
			"status": logStatus, "total": log.Total, "succeeded": log.Succeeded,
			"failed": log.Failed, "duration_ms": log.DurationMs,
			"message": truncate(log.Message, 512), "market": log.Market,
			"trigger_source": log.TriggerSource, "user_id": log.UserID,
			"parameter_summary": truncate(log.ParameterSummary, 1024),
			"range_summary":     truncate(log.RangeSummary, 128), "plan_hash": truncate(log.PlanHash, 64),
		}
		res := tx.Model(&model.DataSyncLog{}).Where("id = ? AND job_run_id = ?", *run.ResultID, run.ID).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("数据同步结果引用无效")
		}
		_ = code
		_ = now
		return nil
	}
	return durableJobBinding{
		resultType: JobResultDataSync,
		create: func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
			var req DataSyncJobRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return 0, err
			}
			log := newSyncLog(req.Task, req.Market, syncAuditForRequest(run, req))
			log.JobRunID = &run.ID
			log.Status = "processing"
			log.Message = "任务已进入统一队列"
			if err := tx.Create(log).Error; err != nil {
				return 0, err
			}
			return log.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, now time.Time) error {
			return persist(tx, run, result, result.Status, "", "", now)
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, status, code, message string, now time.Time) error {
			if run.ResultID == nil || *run.ResultID <= 0 {
				return errors.New("数据同步作业缺少结果引用")
			}
			var log model.DataSyncLog
			if err := tx.Where("id = ? AND job_run_id = ?", *run.ResultID, run.ID).First(&log).Error; err != nil {
				return err
			}
			log.Status = status
			if status == model.JobStatusFailed && log.Failed == 0 {
				log.Failed = 1
			}
			log.Message = sanitizeJobError(message)
			fallback := DurableJobResult{Value: &log}
			return persist(tx, run, fallback, status, code, message, now)
		},
		persistFailure: persist,
	}
}

func syncAuditForRequest(run *model.JobRun, req DataSyncJobRequest) SyncAudit {
	audit := SyncAudit{TriggerSource: req.TriggerSource, ParameterSummary: req.ParameterSummary, PlanHash: req.Maintenance.PlanHash}
	if req.HasMaintenance && (req.Maintenance.From != "" || req.Maintenance.To != "") {
		audit.RangeSummary = req.Maintenance.From + ".." + req.Maintenance.To
	}
	if run != nil && run.TriggeredBy != nil {
		audit.UserID = *run.TriggeredBy
	}
	return audit
}

func normalizeDataSyncRequest(req DataSyncJobRequest) (DataSyncJobRequest, error) {
	if req.Version == 0 {
		req.Version = dataSyncJobSnapshotVersion
	}
	if req.Version != dataSyncJobSnapshotVersion || !isSystemDurableJobKind(req.Task) {
		return req, errors.New("数据同步任务快照版本或类型无效")
	}
	if req.Market == "" {
		req.Market = "cn"
	}
	req.Market = strings.ToLower(strings.TrimSpace(req.Market))
	if req.Market != "cn" {
		return req, errors.New("当前仅支持 cn 市场")
	}
	if req.TriggerSource == "" {
		req.TriggerSource = "scheduler"
	}
	if req.TriggerSource != "scheduler" && req.TriggerSource != "startup" && req.TriggerSource != "admin" && req.TriggerSource != "admin_legacy" && req.TriggerSource != "system" {
		return req, errors.New("无效的触发来源")
	}
	req.ParameterSummary = truncate(req.ParameterSummary, 512)
	req.Reason = truncate(req.Reason, 128)
	return req, nil
}

func (s *MarketService) dataSyncJobHandler(ctx context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
	var req DataSyncJobRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return DurableJobResult{}, err
	}
	var err error
	req, err = normalizeDataSyncRequest(req)
	if err != nil || req.Task == "" {
		return DurableJobResult{}, err
	}
	if err := JobStepTransition(ctx, req.Task); err != nil {
		return DurableJobResult{}, err
	}
	var log *model.DataSyncLog
	audit := SyncAudit{TriggerSource: req.TriggerSource, ParameterSummary: req.ParameterSummary}
	if execution, ok := currentJobExecution(ctx); ok {
		var run model.JobRun
		if common.DB.Select("id", "triggered_by").First(&run, execution.jobID).Error == nil && run.TriggeredBy != nil {
			audit.UserID = *run.TriggeredBy
		}
	}
	switch req.Task {
	case JobKindSyncDailyBars:
		if req.HasMaintenance {
			log, err = s.RunSyncBarsPlan(ctx, req.Maintenance, audit)
		} else {
			log, err = s.SyncTrackedDailyBarsWithAudit(ctx, req.Market, req.BarLimit, audit)
		}
	case JobKindBackfillCalendar:
		if req.HasMaintenance {
			log, err = s.RunCalendarPlan(ctx, req.Maintenance, audit)
		} else {
			log, err = s.BackfillCalendarWithAudit(ctx, req.Market, audit)
		}
	case JobKindSnapshotMarket:
		_, log, err = s.runSnapshotMarketWithAudit(ctx, req.Market, audit)
	case JobKindSyncMarketWide:
		if req.HasMaintenance {
			log, err = s.RunMarketWidePlan(ctx, req.Maintenance, audit)
		} else {
			log, err = s.SyncMarketWideWithAudit(ctx, audit)
		}
		if err == nil && log != nil && log.Succeeded > 0 {
			var pending int64
			common.DB.Model(&model.MarketSyncState{}).
				Where("market = ? AND init_status = ?", req.Market, "pending").Count(&pending)
			if pending > 0 {
				ScheduleSystemDataSyncJob(JobKindInitMarketHistory, DataSyncJobRequest{
					Version: dataSyncJobSnapshotVersion, Market: req.Market, TriggerSource: "system",
					ParameterSummary: fmt.Sprintf("pending=%d", pending), Reason: "全市场增量完成",
				})
			}
		}
	case JobKindInitMarketHistory:
		log, err = s.RunMarketWideInit(ctx, audit)
	case JobKindFactorRebuild:
		start := time.Now()
		table, buildErr := RebuildFactorTable(ctx, req.Reason)
		err = buildErr
		log = newSyncLog(JobKindFactorRebuild, req.Market, audit)
		log.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			log.Status, log.Failed, log.Message = "failed", 1, truncate(err.Error(), 512)
		} else {
			log.Status, log.Total, log.Succeeded = "success", table.Len(), table.Len()
			log.RangeSummary = table.TradeDate
			log.Message = fmt.Sprintf("因子宽表 %s 共 %d 只", table.TradeDate, table.Len())
			// 因子宽表与不可变快照已经成功发布，随后提交全局一次的候选发现。
			// 推荐生成只读发现表，永远不会在用户请求路径触发全市场扫描。
			ScheduleDailyDiscovery(table.TradeDate, "factor_rebuild")
		}
		s.recordSyncLog(ctx, log)
	}
	if log == nil {
		log = newSyncLog(req.Task, req.Market, audit)
		log.Status, log.Failed = "failed", 1
		if err != nil {
			log.Message = truncate(err.Error(), 512)
		}
	}
	if err == nil && log.Status == "failed" {
		err = errors.New(log.Message)
	}
	result := DurableJobResult{Value: log, Total: log.Total, Succeeded: log.Succeeded, Failed: log.Failed}
	switch log.Status {
	case "success":
		result.Status = model.JobStatusSuccess
	case "partial":
		result.Status = model.JobStatusDegraded
	default:
		result.Status = model.JobStatusFailed
	}
	return result, err
}

func StartSystemDataSyncJob(task string, triggeredBy *int64, req DataSyncJobRequest) (*JobRunView, bool, error) {
	run, created, err := startSystemDataSyncJobWithStatus(defaultJobRuntime, task, triggeredBy, req)
	if err != nil {
		return nil, false, err
	}
	return jobRunView(*run), created, nil
}

func startSystemDataSyncJob(runtime *jobRuntime, task string, triggeredBy *int64, req DataSyncJobRequest) (*model.JobRun, error) {
	run, _, err := startSystemDataSyncJobWithStatus(runtime, task, triggeredBy, req)
	return run, err
}

func startSystemDataSyncJobWithStatus(runtime *jobRuntime, task string, triggeredBy *int64, req DataSyncJobRequest) (*model.JobRun, bool, error) {
	if runtime == nil {
		return nil, false, errors.New("作业运行时不可用")
	}
	req.Task = task
	normalized, err := normalizeDataSyncRequest(req)
	if err != nil {
		return nil, false, err
	}
	return runtime.startSystemWithBindingStatus(triggeredBy, task, normalized, nil)
}

// ScheduleSystemDataSyncJob 是定时器入口。队列背压时保留一个去重重试器，直到任务
// 被接受或出现不可重试错误；不会提前创建 JobRun 或 processing 日志。
func ScheduleSystemDataSyncJob(task string, req DataSyncJobRequest) {
	scheduleSystemDataSyncJob(defaultJobRuntime, task, req)
}

func scheduleSystemDataSyncJob(runtime *jobRuntime, task string, req DataSyncJobRequest) {
	if _, err := startSystemDataSyncJob(runtime, task, nil, req); err == nil {
		return
	} else if !errors.Is(err, ErrJobQueueBusy) {
		common.SysWarn("系统任务提交失败 task=%s: %v", task, err)
		return
	}
	encoded, _ := json.Marshal(req)
	key := task + ":" + strconv.FormatUint(uint64(fnv32(encoded)), 16)
	systemScheduleMu.Lock()
	if _, exists := systemSchedulePending[key]; exists {
		systemScheduleMu.Unlock()
		return
	}
	systemSchedulePending[key] = struct{}{}
	systemScheduleMu.Unlock()
	go func() {
		defer func() {
			systemScheduleMu.Lock()
			delete(systemSchedulePending, key)
			systemScheduleMu.Unlock()
		}()
		for {
			time.Sleep(systemScheduleRetryDelay)
			_, retryErr := startSystemDataSyncJob(runtime, task, nil, req)
			if retryErr == nil {
				return
			}
			if !errors.Is(retryErr, ErrJobQueueBusy) {
				common.SysWarn("系统任务背压重试停止 task=%s: %v", task, retryErr)
				return
			}
		}
	}()
}

func fnv32(value []byte) uint32 {
	var hash uint32 = 2166136261
	for _, b := range value {
		hash ^= uint32(b)
		hash *= 16777619
	}
	return hash
}

func CancelActiveSystemJob(actorID int64, kind string) (*JobRunView, error) {
	if !isSystemDurableJobKind(kind) || !isAdminUser(actorID) {
		return nil, ErrJobNotFound
	}
	var run model.JobRun
	err := common.DB.Where("owner_type = ? AND user_id IS NULL AND kind = ? AND status IN ?",
		model.JobOwnerSystem, kind, []string{model.JobStatusQueued, model.JobStatusRunning}).
		Order("id DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return CancelJobRun(actorID, run.ID)
}
