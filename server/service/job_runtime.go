package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	JobKindQA             = "qa"
	JobKindCompare        = "compare"
	JobKindPositionAdvice = "position_advice"
	JobKindScreenerParse  = "screener_parse"

	jobSnapshotVersion  = 1
	jobSnapshotMaxBytes = 16 << 10
	jobWorkerCount      = 4
	jobCapacity         = 32
	jobCancelPoll       = 250 * time.Millisecond

	JobErrorBusy               = "job_queue_busy"
	JobErrorCanceled           = "job_canceled"
	JobErrorInterrupted        = "job_interrupted"
	JobErrorHandlerUnavailable = "job_handler_unavailable"
	JobErrorSnapshotInvalid    = "job_snapshot_invalid"
)

var (
	ErrJobQueueBusy         error = &jobServiceError{code: JobErrorBusy, message: "作业队列已满，请稍后重试"}
	ErrJobKindUnsupported         = errors.New("不支持重跑该任务类型")
	ErrJobNotFound                = errors.New("作业不存在")
	ErrJobNotCancelable           = errors.New("作业已结束，不能取消")
	ErrJobSnapshotTooLarge        = fmt.Errorf("%s: 作业请求快照超过 %d 字节", JobErrorSnapshotInvalid, jobSnapshotMaxBytes)
	ErrJobSnapshotSensitive       = fmt.Errorf("%s: 作业请求快照包含禁止持久化的字段", JobErrorSnapshotInvalid)
	errJobCancelWon               = errors.New("job cancel won")
)

type jobServiceError struct {
	code    string
	message string
}

func (e *jobServiceError) Error() string       { return e.message }
func (e *jobServiceError) RefusalCode() string { return e.code }

type jobPanicError struct{ value any }

func (e *jobPanicError) Error() string { return fmt.Sprintf("任务异常终止: %v", e.value) }

type persistedJobSnapshot struct {
	Version      int             `json:"version"`
	Kind         string          `json:"kind"`
	AllowPrivate bool            `json:"allow_private"`
	Request      json.RawMessage `json:"request"`
}

// DurableJobResult 将原业务结果与作业观测元数据分开。Value 只写入 llm_tasks
// 兼容结果行，不会进入 job_runs。
type DurableJobResult struct {
	Value            any
	Status           string
	TraceID          string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Total            int
	Succeeded        int
	Failed           int
}

type DurableJobHandler func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error)

type durableJobHandler struct {
	timeout time.Duration
	run     DurableJobHandler
}

type jobRuntime struct {
	workers     int
	queue       chan int64
	slots       chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	startOnce   sync.Once
	recoverOnce sync.Once
	workerWG    sync.WaitGroup

	mu        sync.Mutex
	handlers  map[string]durableJobHandler
	overrides map[int64]durableJobHandler
	scheduled map[int64]struct{}
	cancels   map[int64]context.CancelFunc
	createMu  sync.Mutex
}

func newJobRuntime(workers, capacity int) *jobRuntime {
	if workers <= 0 {
		workers = 1
	}
	if capacity < workers {
		capacity = workers
	}
	return &jobRuntime{
		workers:   workers,
		queue:     make(chan int64, capacity),
		slots:     make(chan struct{}, capacity),
		stop:      make(chan struct{}),
		handlers:  make(map[string]durableJobHandler),
		overrides: make(map[int64]durableJobHandler),
		scheduled: make(map[int64]struct{}),
		cancels:   make(map[int64]context.CancelFunc),
	}
}

var defaultJobRuntime = newJobRuntime(jobWorkerCount, jobCapacity)

// RegisterDurableLLMJobHandler 注册可从持久快照恢复的处理器。四类首批任务由各自
// service 构造函数注册，重启恢复与失败重跑只接受这些已注册类型。
func RegisterDurableLLMJobHandler(kind string, timeout time.Duration, handler DurableJobHandler) {
	if !isDurableJobKind(kind) {
		return
	}
	defaultJobRuntime.register(kind, timeout, handler)
}

func (r *jobRuntime) register(kind string, timeout time.Duration, handler DurableJobHandler) {
	kind = strings.TrimSpace(kind)
	if kind == "" || timeout <= 0 || handler == nil {
		return
	}
	r.mu.Lock()
	r.handlers[kind] = durableJobHandler{timeout: timeout, run: handler}
	r.mu.Unlock()
}

// StartJobRuntime 在四类 handler 完成注册后调用。running 作业在启动边界收敛，
// queued 作业按 ID 升序重新进入有界队列；超出容量的行保持 queued，槽位释放后续排。
func StartJobRuntime() {
	defaultJobRuntime.startWorkers()
	defaultJobRuntime.recoverOnce.Do(func() {
		if err := defaultJobRuntime.recoverPersisted(); err != nil {
			common.SysWarn("统一作业恢复失败: %v", err)
		}
	})
}

func (r *jobRuntime) startWorkers() {
	r.startOnce.Do(func() {
		for i := 0; i < r.workers; i++ {
			r.workerWG.Add(1)
			go func() {
				defer r.workerWG.Done()
				r.worker()
			}()
		}
	})
}

func (r *jobRuntime) close() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.workerWG.Wait()
}

func (r *jobRuntime) worker() {
	for {
		select {
		case <-r.stop:
			return
		case jobID := <-r.queue:
			r.execute(jobID)
			r.release(jobID)
			r.schedulePersistedQueued()
		}
	}
}

func (r *jobRuntime) reserve() bool {
	select {
	case r.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *jobRuntime) enqueueReserved(jobID int64) {
	r.mu.Lock()
	r.scheduled[jobID] = struct{}{}
	r.mu.Unlock()
	r.queue <- jobID
}

func (r *jobRuntime) release(jobID int64) {
	r.mu.Lock()
	delete(r.scheduled, jobID)
	delete(r.overrides, jobID)
	delete(r.cancels, jobID)
	r.mu.Unlock()
	<-r.slots
}

func (r *jobRuntime) handlerFor(jobID int64, kind string) (durableJobHandler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if handler, ok := r.overrides[jobID]; ok {
		return handler, true
	}
	handler, ok := r.handlers[kind]
	return handler, ok
}

func (r *jobRuntime) hasHandler(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.handlers[kind]
	return ok
}

func (r *jobRuntime) setCancel(jobID int64, cancel context.CancelFunc) {
	r.mu.Lock()
	r.cancels[jobID] = cancel
	r.mu.Unlock()
}

func (r *jobRuntime) signalCancel(jobID int64) {
	r.mu.Lock()
	cancel := r.cancels[jobID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func makeJobSnapshot(kind string, request any, allowPrivate bool) ([]byte, string, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, "", fmt.Errorf("任务请求无法序列化: %w", err)
	}
	if err := validateJobRequestSnapshot(requestJSON); err != nil {
		return nil, "", err
	}
	snapshotJSON, err := json.Marshal(persistedJobSnapshot{
		Version: jobSnapshotVersion, Kind: kind, AllowPrivate: allowPrivate,
		Request: append(json.RawMessage(nil), requestJSON...),
	})
	if err != nil {
		return nil, "", fmt.Errorf("任务快照无法序列化: %w", err)
	}
	if len(snapshotJSON) > jobSnapshotMaxBytes {
		return nil, "", ErrJobSnapshotTooLarge
	}
	sum := sha256.Sum256(requestJSON)
	return snapshotJSON, hex.EncodeToString(sum[:]), nil
}

func validateJobRequestSnapshot(raw []byte) error {
	if len(raw) > jobSnapshotMaxBytes {
		return ErrJobSnapshotTooLarge
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s: %w", JobErrorSnapshotInvalid, err)
	}
	if snapshotContainsSensitiveField(value) {
		return ErrJobSnapshotSensitive
	}
	return nil
}

func snapshotContainsSensitiveField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
			switch normalized {
			case "apikey", "authorization", "systemprompt", "systemmessage", "messages",
				"datasnapshot", "resultsnapshot", "resultjson", "secret", "accesstoken", "refreshtoken":
				return true
			}
			if snapshotContainsSensitiveField(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if snapshotContainsSensitiveField(child) {
				return true
			}
		}
	}
	return false
}

func sanitizeJobError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "作业执行失败"
	}
	lower := strings.ToLower(message)
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(lower)
	for _, marker := range []string{"authorization", "bearer ", "sk-"} {
		if strings.Contains(lower, marker) {
			return "作业失败（敏感错误详情已隐藏）"
		}
	}
	for _, marker := range []string{"apikey", "accesstoken", "refreshtoken", "systemprompt", "systemmessage"} {
		if strings.Contains(compact, marker) {
			return "作业失败（敏感错误详情已隐藏）"
		}
	}
	return truncateRunes(message, 512)
}

func decodePersistedJobSnapshot(run model.JobRun) (persistedJobSnapshot, error) {
	if len(run.RequestSnapshot) == 0 || len(run.RequestSnapshot) > jobSnapshotMaxBytes {
		return persistedJobSnapshot{}, ErrJobSnapshotTooLarge
	}
	var snapshot persistedJobSnapshot
	if err := json.Unmarshal([]byte(run.RequestSnapshot), &snapshot); err != nil {
		return snapshot, fmt.Errorf("%s: %w", JobErrorSnapshotInvalid, err)
	}
	if snapshot.Version != jobSnapshotVersion || snapshot.Kind != run.Kind || len(snapshot.Request) == 0 {
		return snapshot, fmt.Errorf("%s: 快照版本或类型不匹配", JobErrorSnapshotInvalid)
	}
	if err := validateJobRequestSnapshot(snapshot.Request); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func activeJobKey(userID int64, kind, requestHash string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", userID, kind, requestHash)))
	return hex.EncodeToString(sum[:])
}

func (r *jobRuntime) start(userID int64, kind string, request any, allowPrivate bool, parentID *int64, override *durableJobHandler) (*LLMTaskView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if userID <= 0 {
		return nil, errors.New("非法的用户 ID")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" || len(kind) > 64 {
		return nil, errors.New("任务类型不能为空且不能超过 64 个字符")
	}
	if override == nil && !r.hasHandler(kind) {
		return nil, ErrJobKindUnsupported
	}
	snapshotJSON, requestHash, err := makeJobSnapshot(kind, request, allowPrivate)
	if err != nil {
		return nil, err
	}

	if view, found, err := findActiveJobResult(userID, kind, requestHash); err != nil || found {
		return view, err
	}
	r.startWorkers()
	if !r.reserve() {
		return nil, ErrJobQueueBusy
	}

	r.createMu.Lock()
	defer r.createMu.Unlock()
	if view, found, err := findActiveJobResult(userID, kind, requestHash); err != nil || found {
		<-r.slots
		return view, err
	}

	now := time.Now()
	activeKey := activeJobKey(userID, kind, requestHash)
	run := model.JobRun{
		UserID: userID, Kind: kind, RequestHash: requestHash, ActiveKey: &activeKey,
		ParentID: parentID,
		Status:   model.JobStatusQueued, SnapshotVersion: jobSnapshotVersion,
		RequestSnapshot: string(snapshotJSON), ResultType: "llm_task", QueuedAt: now,
	}
	var task model.LLMTask
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		task = model.LLMTask{
			UserID: userID, Kind: kind, JobRunID: &run.ID, RequestHash: requestHash,
			Status: model.LLMTaskStatusProcessing,
		}
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.JobRun{}).Where("id = ?", run.ID).
			Update("result_id", task.ID).Error; err != nil {
			return err
		}
		run.ResultID = &task.ID
		started := now
		step := model.JobStep{JobRunID: run.ID, Sequence: 1, Name: model.JobStepQueued,
			Status: model.JobStatusRunning, StartedAt: &started}
		if err := tx.Create(&step).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "created", run.Status)
	})
	if err != nil {
		<-r.slots
		if view, found, queryErr := findActiveJobResult(userID, kind, requestHash); queryErr == nil && found {
			return view, nil
		}
		return nil, err
	}
	if override != nil {
		r.mu.Lock()
		r.overrides[run.ID] = *override
		r.mu.Unlock()
	}
	r.enqueueReserved(run.ID)
	return llmTaskView(task, false), nil
}

func findActiveJobResult(userID int64, kind, requestHash string) (*LLMTaskView, bool, error) {
	var run model.JobRun
	err := common.DB.Where("user_id = ? AND kind = ? AND request_hash = ? AND status IN ?",
		userID, kind, requestHash, []string{model.JobStatusQueued, model.JobStatusRunning}).
		Order("id DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if run.ResultID == nil {
		return nil, false, errors.New("在途作业缺少兼容结果引用")
	}
	var task model.LLMTask
	if err := common.DB.Where("id = ? AND user_id = ?", *run.ResultID, userID).First(&task).Error; err != nil {
		return nil, false, err
	}
	return llmTaskView(task, false), true, nil
}

func appendJobEvent(tx *gorm.DB, userID, jobID int64, eventType, status string) error {
	return tx.Create(&model.JobEvent{
		UserID: userID, JobRunID: jobID, Type: eventType, Status: status,
	}).Error
}

func (r *jobRuntime) execute(jobID int64) {
	var run model.JobRun
	if err := common.DB.First(&run, jobID).Error; err != nil {
		return
	}
	handler, ok := r.handlerFor(jobID, run.Kind)
	if !ok {
		r.failQueued(jobID, JobErrorHandlerUnavailable, "作业处理器不可用")
		return
	}
	if !r.claim(run) {
		return
	}
	if err := common.DB.First(&run, jobID).Error; err != nil {
		return
	}
	snapshot, err := decodePersistedJobSnapshot(run)
	if err != nil {
		r.finishFailed(run, JobErrorSnapshotInvalid, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), handler.timeout)
	r.setCancel(jobID, cancel)
	monitorDone := make(chan struct{})
	go r.monitorCancellation(ctx, cancel, jobID, monitorDone)
	started := time.Now()
	result, runErr := r.callHandler(ctx, handler, run.UserID, snapshot)
	if runErr == nil && ctx.Err() != nil {
		runErr = ctx.Err()
	}
	cancel()
	close(monitorDone)
	latency := time.Since(started).Milliseconds()

	if runErr != nil {
		requested := jobCancelRequested(jobID)
		if requested {
			r.finishCanceled(run)
			return
		}
		code := asyncLLMTaskErrorCode(runErr)
		var panicErr *jobPanicError
		if errors.As(runErr, &panicErr) {
			code = AsyncLLMTaskErrorPanic
		}
		if errors.Is(runErr, context.Canceled) {
			code = AsyncLLMTaskErrorFailed
		}
		r.finishFailed(run, code, runErr.Error())
		return
	}
	if jobCancelRequested(jobID) {
		r.finishCanceled(run)
		return
	}
	result.Status = normalizeJobResultStatus(result.Status)
	if result.Status == "" {
		r.finishFailed(run, AsyncLLMTaskErrorFailed, "作业返回了非法终态")
		return
	}
	resultJSON, err := json.Marshal(result.Value)
	if err != nil {
		r.finishFailed(run, AsyncLLMTaskErrorResultEncode, "任务结果无法序列化: "+err.Error())
		return
	}
	result = fillJobUsageFromTrace(run.UserID, result)
	if err := r.persistSuccess(run, result, resultJSON, latency); err != nil {
		if errors.Is(err, errJobCancelWon) {
			r.finishCanceled(run)
			return
		}
		common.SysWarn("作业结果持久化失败 job=%d: %v", run.ID, err)
		r.finishFailed(run, AsyncLLMTaskErrorFailed, "作业结果持久化失败")
	}
}

func (r *jobRuntime) callHandler(ctx context.Context, handler durableJobHandler, userID int64, snapshot persistedJobSnapshot) (result DurableJobResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &jobPanicError{value: recovered}
			result = DurableJobResult{}
		}
	}()
	return handler.run(ctx, userID, snapshot.AllowPrivate, append(json.RawMessage(nil), snapshot.Request...))
}

func (r *jobRuntime) monitorCancellation(ctx context.Context, cancel context.CancelFunc, jobID int64, done <-chan struct{}) {
	ticker := time.NewTicker(jobCancelPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if jobCancelRequested(jobID) {
				cancel()
				return
			}
		}
	}
}

func jobCancelRequested(jobID int64) bool {
	var run model.JobRun
	if err := common.DB.Select("cancel_requested").First(&run, jobID).Error; err != nil {
		return false
	}
	return run.CancelRequested
}

func normalizeJobResultStatus(status string) string {
	switch status {
	case "", model.JobStatusSuccess:
		return model.JobStatusSuccess
	case model.JobStatusDegraded:
		return model.JobStatusDegraded
	default:
		return ""
	}
}

func fillJobUsageFromTrace(userID int64, result DurableJobResult) DurableJobResult {
	if strings.TrimSpace(result.TraceID) == "" || common.DB == nil {
		return result
	}
	var totals struct {
		PromptTokens     int
		CompletionTokens int
		TotalTokens      int
	}
	if err := common.DB.Model(&model.LLMCallLog{}).
		Select("COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, COALESCE(SUM(completion_tokens), 0) AS completion_tokens, COALESCE(SUM(total_tokens), 0) AS total_tokens").
		Where("user_id = ? AND trace_id = ?", userID, result.TraceID).Scan(&totals).Error; err == nil {
		if result.PromptTokens == 0 {
			result.PromptTokens = totals.PromptTokens
		}
		if result.CompletionTokens == 0 {
			result.CompletionTokens = totals.CompletionTokens
		}
		if result.TotalTokens == 0 {
			result.TotalTokens = totals.TotalTokens
		}
	}
	return result
}

func (r *jobRuntime) claim(run model.JobRun) bool {
	now := time.Now()
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).
			Where("id = ? AND status = ? AND cancel_requested = ?", run.ID, model.JobStatusQueued, false).
			Updates(map[string]any{"status": model.JobStatusRunning, "started_at": now, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND name = ? AND status = ?", run.ID, model.JobStepQueued, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSuccess, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		dispatchStarted := now
		dispatchFinished := time.Now()
		steps := []model.JobStep{
			{JobRunID: run.ID, Sequence: 2, Name: model.JobStepDispatch, Status: model.JobStatusSuccess,
				StartedAt: &dispatchStarted, FinishedAt: &dispatchFinished},
			{JobRunID: run.ID, Sequence: 3, Name: model.JobStepExecute, Status: model.JobStatusRunning,
				StartedAt: &dispatchFinished},
		}
		if err := tx.Create(&steps).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusRunning)
	})
	return err == nil
}

func (r *jobRuntime) beginPersist(run model.JobRun) error {
	now := time.Now()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		var current model.JobRun
		if err := tx.Select("id", "status", "cancel_requested").First(&current, run.ID).Error; err != nil {
			return err
		}
		if current.Status != model.JobStatusRunning || current.CancelRequested {
			return errJobCancelWon
		}
		if err := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND name = ? AND status = ?", run.ID, model.JobStepExecute, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSuccess, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		started := now
		if err := tx.Create(&model.JobStep{JobRunID: run.ID, Sequence: 4, Name: model.JobStepPersist,
			Status: model.JobStatusRunning, StartedAt: &started}).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "step", model.JobStatusRunning)
	})
}

func (r *jobRuntime) persistSuccess(run model.JobRun, result DurableJobResult, resultJSON []byte, latency int64) error {
	if err := r.beginPersist(run); err != nil {
		return err
	}
	now := time.Now()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"status": result.Status, "active_key": nil, "error": "", "error_code": "",
			"trace_id": result.TraceID, "provider": result.Provider, "model": result.Model,
			"prompt_tokens": result.PromptTokens, "completion_tokens": result.CompletionTokens,
			"total_tokens": result.TotalTokens, "latency_ms": latency, "total": result.Total,
			"succeeded": result.Succeeded, "failed": result.Failed,
			"finished_at": now, "updated_at": now,
		}
		res := tx.Model(&model.JobRun{}).
			Where("id = ? AND status = ? AND cancel_requested = ?", run.ID, model.JobStatusRunning, false).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errJobCancelWon
		}
		if run.ResultID == nil {
			return errors.New("作业缺少结果引用")
		}
		if err := tx.Model(&model.LLMTask{}).
			Where("id = ? AND user_id = ? AND job_run_id = ?", *run.ResultID, run.UserID, run.ID).
			Updates(map[string]any{
				"status": model.LLMTaskStatusSuccess, "result_json": string(resultJSON),
				"error": "", "error_code": "", "active_key": nil, "updated_at": now,
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND name = ? AND status = ?", run.ID, model.JobStepPersist, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSuccess, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", result.Status)
	})
}

func (r *jobRuntime) finishFailed(run model.JobRun, code, message string) {
	if jobCancelRequested(run.ID) {
		r.finishCanceled(run)
		return
	}
	now := time.Now()
	safeMessage := sanitizeJobError(message)
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).
			Where("id = ? AND status = ? AND cancel_requested = ?", run.ID, model.JobStatusRunning, false).
			Updates(map[string]any{
				"status": model.JobStatusFailed, "active_key": nil,
				"error": safeMessage, "error_code": code,
				"finished_at": now, "updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errJobCancelWon
		}
		if run.ResultID != nil {
			if err := failCompatibleLLMTask(tx, *run.ResultID, run.UserID, code, message, now); err != nil {
				return err
			}
		}
		if err := finishRunningJobSteps(tx, run.ID, model.JobStatusFailed, code, message, now); err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusFailed)
	})
	if errors.Is(err, errJobCancelWon) {
		r.finishCanceled(run)
	} else if err != nil {
		common.SysWarn("作业失败状态回写失败 job=%d: %v", run.ID, err)
	}
}

func (r *jobRuntime) finishCanceled(run model.JobRun) {
	now := time.Now()
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).
			Where("id = ? AND status = ?", run.ID, model.JobStatusRunning).
			Updates(map[string]any{
				"status": model.JobStatusCanceled, "cancel_requested": true, "active_key": nil,
				"error": "作业已取消", "error_code": JobErrorCanceled,
				"finished_at": now, "updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return nil
		}
		if run.ResultID != nil {
			if err := failCompatibleLLMTask(tx, *run.ResultID, run.UserID, JobErrorCanceled, "作业已取消", now); err != nil {
				return err
			}
		}
		if err := finishRunningJobSteps(tx, run.ID, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusCanceled)
	})
	if err != nil {
		common.SysWarn("作业取消状态回写失败 job=%d: %v", run.ID, err)
	}
}

func finishRunningJobSteps(tx *gorm.DB, jobID int64, status, code, message string, now time.Time) error {
	return tx.Model(&model.JobStep{}).
		Where("job_run_id = ? AND status = ?", jobID, model.JobStatusRunning).
		Updates(map[string]any{
			"status": status, "error": sanitizeJobError(message), "error_code": code,
			"finished_at": now, "updated_at": now,
		}).Error
}

func failCompatibleLLMTask(tx *gorm.DB, taskID, userID int64, code, message string, now time.Time) error {
	return tx.Model(&model.LLMTask{}).
		Where("id = ? AND user_id = ? AND status = ?", taskID, userID, model.LLMTaskStatusProcessing).
		Updates(map[string]any{
			"status": model.LLMTaskStatusFailed, "active_key": nil,
			"error": sanitizeJobError(message), "error_code": code, "updated_at": now,
		}).Error
}

func (r *jobRuntime) failQueued(jobID int64, code, message string) {
	var run model.JobRun
	if err := common.DB.First(&run, jobID).Error; err != nil {
		return
	}
	now := time.Now()
	safeMessage := sanitizeJobError(message)
	_ = common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).Where("id = ? AND status = ?", jobID, model.JobStatusQueued).
			Updates(map[string]any{
				"status": model.JobStatusFailed, "active_key": nil, "error": safeMessage,
				"error_code": code, "finished_at": now, "updated_at": now,
			})
		if res.Error != nil || res.RowsAffected != 1 {
			return res.Error
		}
		if run.ResultID != nil {
			if err := failCompatibleLLMTask(tx, *run.ResultID, run.UserID, code, message, now); err != nil {
				return err
			}
		}
		if err := finishRunningJobSteps(tx, jobID, model.JobStatusFailed, code, message, now); err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusFailed)
	})
}

func (r *jobRuntime) recoverPersisted() error {
	if common.DB == nil {
		return errors.New("数据库尚未初始化")
	}
	var running []model.JobRun
	if err := common.DB.Where("status = ?", model.JobStatusRunning).Order("id ASC").Find(&running).Error; err != nil {
		return err
	}
	for _, run := range running {
		if run.CancelRequested {
			r.finishCanceled(run)
		} else {
			r.finishFailed(run, JobErrorInterrupted, "作业因服务重启中断，请重跑")
		}
	}
	r.schedulePersistedQueued()
	return nil
}

func (r *jobRuntime) schedulePersistedQueued() {
	if common.DB == nil {
		return
	}
	var queued []model.JobRun
	limit := cap(r.slots)
	if err := common.DB.Select("id", "kind", "cancel_requested").
		Where("status = ?", model.JobStatusQueued).Order("id ASC").Limit(limit).Find(&queued).Error; err != nil {
		common.SysWarn("读取待恢复作业失败: %v", err)
		return
	}
	for _, run := range queued {
		r.mu.Lock()
		_, scheduled := r.scheduled[run.ID]
		r.mu.Unlock()
		if scheduled {
			continue
		}
		if run.CancelRequested {
			r.cancelQueued(run.ID)
			continue
		}
		if !r.hasHandler(run.Kind) {
			r.failQueued(run.ID, JobErrorHandlerUnavailable, "作业类型没有可恢复处理器")
			continue
		}
		if !r.reserve() {
			return
		}
		r.enqueueReserved(run.ID)
	}
}

func (r *jobRuntime) cancelQueued(jobID int64) {
	var run model.JobRun
	if err := common.DB.First(&run, jobID).Error; err != nil {
		return
	}
	now := time.Now()
	_ = common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).Where("id = ? AND status = ?", jobID, model.JobStatusQueued).
			Updates(map[string]any{
				"status": model.JobStatusCanceled, "cancel_requested": true, "active_key": nil,
				"error": "作业已取消", "error_code": JobErrorCanceled,
				"finished_at": now, "updated_at": now,
			})
		if res.Error != nil || res.RowsAffected != 1 {
			return res.Error
		}
		if run.ResultID != nil {
			if err := failCompatibleLLMTask(tx, *run.ResultID, run.UserID, JobErrorCanceled, "作业已取消", now); err != nil {
				return err
			}
		}
		if err := finishRunningJobSteps(tx, run.ID, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusCanceled)
	})
}
