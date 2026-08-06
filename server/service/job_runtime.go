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
	JobKindAnalysis       = "analysis"
	JobKindRecommendation = "recommendation"
	JobKindDailyReport    = "daily_report"

	JobResultLLMTask        = "llm_task"
	JobResultAnalysis       = "analysis"
	JobResultRecommendation = "recommendation"
	JobResultDailyReport    = "daily_report"

	jobSnapshotVersion  = 1
	jobSnapshotMaxBytes = 16 << 10
	jobWorkerCount      = 4
	jobCapacity         = 32
	jobCancelPoll       = 250 * time.Millisecond

	JobErrorBusy               = "job_queue_busy"
	JobErrorAlreadyRunning     = "job_already_running"
	JobErrorCanceled           = "job_canceled"
	JobErrorInterrupted        = "job_interrupted"
	JobErrorHandlerUnavailable = "job_handler_unavailable"
	JobErrorSnapshotInvalid    = "job_snapshot_invalid"
)

var (
	ErrJobQueueBusy         error = &jobServiceError{code: JobErrorBusy, message: "作业队列已满，请稍后重试"}
	ErrJobAlreadyRunning          = &jobServiceError{code: JobErrorAlreadyRunning, message: "相同请求已有任务运行中，请等待其结束后再重跑"}
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
	Version int             `json:"version"`
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request"`
}

// DurableJobResult 将原业务结果与作业观测元数据分开。Value 只由兼容的
// llm_task 绑定使用；分析/推荐/日报结果继续由各自业务表承载。
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

// durableJobBinding 把作业事实与业务结果载体连接起来。创建、失败/取消回写和
// 成功引用校验都在运行时事务内执行，避免孤儿业务行或 JobRun 成功但结果缺失。
type durableJobBinding struct {
	resultType string
	create     func(*gorm.DB, *model.JobRun, json.RawMessage) (int64, error)
	// resultCommittedByHandler 表示业务处理器返回前已把结果事实写入原业务表。
	// 此时迟到的 cancel_requested 不能再把 JobRun 改成 canceled，否则会形成
	// “作业取消但结果成功”的半终态；最终成功事务会校验结果并让成功获胜。
	resultCommittedByHandler bool
	// persistSuccess 必须在 JobRun 终态 CAS 同一事务内确认业务结果已经存在且可读。
	persistSuccess func(*gorm.DB, *model.JobRun, DurableJobResult, []byte, time.Time) error
	finishFailure  func(*gorm.DB, *model.JobRun, string, string, string, time.Time) error
}

type durableJobHandler struct {
	timeout  time.Duration
	run      DurableJobHandler
	binding  durableJobBinding
	legacyIO bool // 旧四类任务由运行时自动记录 execute 步骤。
}

func defaultLLMTaskBinding(kind string) durableJobBinding {
	return durableJobBinding{
		resultType: JobResultLLMTask,
		create: func(tx *gorm.DB, run *model.JobRun, _ json.RawMessage) (int64, error) {
			task := &model.LLMTask{UserID: run.UserID, Kind: run.Kind, JobRunID: &run.ID,
				RequestHash: run.RequestHash, Status: model.LLMTaskStatusProcessing}
			if err := tx.Create(task).Error; err != nil {
				return 0, err
			}
			return task.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, resultJSON []byte, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("作业缺少兼容结果引用")
			}
			res := tx.Model(&model.LLMTask{}).
				Where("id = ? AND user_id = ? AND job_run_id = ?", *run.ResultID, run.UserID, run.ID).
				Updates(map[string]any{"status": model.LLMTaskStatusSuccess, "result_json": string(resultJSON),
					"error": "", "error_code": "", "active_key": nil, "updated_at": now})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("作业兼容结果引用无效")
			}
			return nil
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, _ string, code, message string, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("作业缺少兼容结果引用")
			}
			return failCompatibleLLMTask(tx, *run.ResultID, run.UserID, code, message, now)
		},
	}
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

// RegisterDurableLLMJobHandler 注册兼容 llm_tasks 的可恢复处理器。
func RegisterDurableLLMJobHandler(kind string, timeout time.Duration, handler DurableJobHandler) {
	defaultJobRuntime.registerWithBinding(kind, timeout, handler, defaultLLMTaskBinding(kind), true)
}

// registerDurableBusinessJobHandler 注册以原业务表为结果事实的作业。结果正文不会
// 被复制进 job_runs；handler 只返回状态和调用元数据。
func registerDurableBusinessJobHandler(kind string, timeout time.Duration, binding durableJobBinding, handler DurableJobHandler) {
	defaultJobRuntime.registerWithBinding(kind, timeout, handler, binding, false)
}

// register 保留测试与旧内部调用的三参数形式，默认使用 llm_task 结果绑定。
func (r *jobRuntime) register(kind string, timeout time.Duration, handler DurableJobHandler) {
	r.registerWithBinding(kind, timeout, handler, defaultLLMTaskBinding(kind), true)
}

func (r *jobRuntime) registerWithBinding(kind string, timeout time.Duration, handler DurableJobHandler, binding durableJobBinding, legacyIO bool) {
	kind = strings.TrimSpace(kind)
	if kind == "" || timeout <= 0 || handler == nil || binding.create == nil || binding.finishFailure == nil || binding.persistSuccess == nil {
		return
	}
	r.mu.Lock()
	r.handlers[kind] = durableJobHandler{timeout: timeout, run: handler, binding: binding, legacyIO: legacyIO}
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

func (r *jobRuntime) bindingForRun(run model.JobRun) durableJobBinding {
	r.mu.Lock()
	if h, ok := r.overrides[run.ID]; ok && h.binding.create != nil {
		r.mu.Unlock()
		return h.binding
	}
	h, ok := r.handlers[run.Kind]
	r.mu.Unlock()
	if ok && h.binding.create != nil {
		return h.binding
	}
	return defaultLLMTaskBinding(run.Kind)
}

func (r *jobRuntime) hasHandler(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.handlers[kind]
	return ok
}

func (r *jobRuntime) handler(kind string) (durableJobHandler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handlers[kind]
	return h, ok
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

// allowPrivate 仅为兼容现有调用签名保留，绝不写入持久快照。执行器会按运行时的
// 当前账号状态重新判定私网权限，避免降权或禁用后仍重放提交时权限。
func makeJobSnapshot(kind string, request any, _ bool) ([]byte, string, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return nil, "", fmt.Errorf("任务请求无法序列化: %w", err)
	}
	if err := validateJobRequestSnapshot(requestJSON); err != nil {
		return nil, "", err
	}
	snapshotJSON, err := json.Marshal(persistedJobSnapshot{
		Version: jobSnapshotVersion, Kind: kind,
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
			case "apikey", "authorization", "cookie", "systemprompt", "prompt", "prompttext", "systemmessage", "messages",
				"data", "datasnapshot", "datasnapshotjson", "result", "resultsnapshot", "resultjson", "resultbody",
				"response", "responsebody", "output", "payload", "body", "requestbody", "secret", "accesstoken",
				"refreshtoken", "allowprivate":
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

// start 保留旧测试与兼容调用签名；业务表任务走 startWithBinding。
func (r *jobRuntime) start(userID int64, kind string, request any, allowPrivate bool, parentID *int64, override *durableJobHandler) (*LLMTaskView, error) {
	run, err := r.startWithBinding(userID, kind, request, allowPrivate, parentID, override, nil)
	if err != nil {
		return nil, err
	}
	if run.ResultID == nil {
		return nil, errors.New("作业缺少兼容结果引用")
	}
	var task model.LLMTask
	if err := common.DB.Where("id = ? AND user_id = ?", *run.ResultID, userID).First(&task).Error; err != nil {
		return nil, err
	}
	return llmTaskView(task, false), nil
}

func (r *jobRuntime) startWithBinding(userID int64, kind string, request any, allowPrivate bool, parentID *int64, override *durableJobHandler, bindingOverride *durableJobBinding) (*model.JobRun, error) {
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
	var registered durableJobHandler
	if override != nil {
		registered = *override
	} else {
		var ok bool
		registered, ok = r.handler(kind)
		if !ok {
			return nil, ErrJobKindUnsupported
		}
	}
	binding := registered.binding
	if bindingOverride != nil {
		binding = *bindingOverride
	}
	if binding.create == nil {
		binding = defaultLLMTaskBinding(kind)
	}
	if binding.resultType == "" {
		binding.resultType = JobResultLLMTask
	}
	if registered.run == nil {
		return nil, ErrJobKindUnsupported
	}
	snapshotJSON, requestHash, err := makeJobSnapshot(kind, request, allowPrivate)
	if err != nil {
		return nil, err
	}

	if active, found, err := findActiveJobRun(userID, kind, requestHash); err != nil || found {
		if parentID != nil && found {
			return nil, ErrJobAlreadyRunning
		}
		return active, err
	}
	r.startWorkers()
	if !r.reserve() {
		return nil, ErrJobQueueBusy
	}

	r.createMu.Lock()
	defer r.createMu.Unlock()
	if active, found, err := findActiveJobRun(userID, kind, requestHash); err != nil || found {
		<-r.slots
		if parentID != nil && found {
			return nil, ErrJobAlreadyRunning
		}
		return active, err
	}

	now := time.Now()
	activeKey := activeJobKey(userID, kind, requestHash)
	run := model.JobRun{
		UserID: userID, Kind: kind, RequestHash: requestHash, ActiveKey: &activeKey,
		ParentID: parentID,
		Status:   model.JobStatusQueued, SnapshotVersion: jobSnapshotVersion,
		RequestSnapshot: string(snapshotJSON), ResultType: binding.resultType, QueuedAt: now,
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		resultID, createErr := binding.create(tx, &run, snapshotJSONRequest(snapshotJSON))
		if createErr != nil {
			return createErr
		}
		if resultID <= 0 {
			return errors.New("业务结果占位创建失败")
		}
		run.ResultID = &resultID
		if err := tx.Model(&model.JobRun{}).Where("id = ? AND result_id IS NULL", run.ID).
			Update("result_id", resultID).Error; err != nil {
			return err
		}
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
		if active, found, queryErr := findActiveJobRun(userID, kind, requestHash); found || queryErr != nil {
			if parentID != nil && found {
				return nil, ErrJobAlreadyRunning
			}
			return active, queryErr
		}
		return nil, err
	}
	if override != nil {
		if override.binding.create == nil {
			override.binding = binding
		}
		r.mu.Lock()
		r.overrides[run.ID] = *override
		r.mu.Unlock()
	}
	r.enqueueReserved(run.ID)
	return &run, nil
}

func snapshotJSONRequest(snapshot []byte) json.RawMessage {
	var persisted persistedJobSnapshot
	if json.Unmarshal(snapshot, &persisted) == nil {
		return append(json.RawMessage(nil), persisted.Request...)
	}
	return nil
}

func findActiveJobForStart(userID int64, kind, requestHash string, retry bool) (*LLMTaskView, bool, error) {
	view, found, err := findActiveJobResult(userID, kind, requestHash)
	if err != nil || !found {
		return view, found, err
	}
	if retry {
		return nil, true, ErrJobAlreadyRunning
	}
	return view, true, nil
}

func findActiveJobRun(userID int64, kind, requestHash string) (*model.JobRun, bool, error) {
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
		return nil, false, errors.New("在途作业缺少结果引用")
	}
	return &run, true, nil
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
	if !r.claim(run, handler.legacyIO) {
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
	ctx = withJobExecution(ctx, jobExecution{runtime: r, jobID: run.ID, resultType: run.ResultType, resultID: run.ResultID})
	r.setCancel(jobID, cancel)
	monitorDone := make(chan struct{})
	go r.monitorCancellation(ctx, cancel, jobID, monitorDone)
	started := time.Now()
	result, runErr := r.callHandler(ctx, handler, run.UserID, snapshot)
	if runErr == nil && ctx.Err() != nil && !handler.binding.resultCommittedByHandler {
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
	// 业务处理器已经完成并写入结果时，成功 CAS 与取消请求竞争；谁先提交谁获胜。
	// 不在这里预判 cancel_requested，避免“结果已存在但作业被取消”的半终态。
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
	if err := r.persistSuccess(run, handler.binding, result, resultJSON, latency); err != nil {
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
	return handler.run(ctx, userID, isAdminUser(userID), append(json.RawMessage(nil), snapshot.Request...))
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

func (r *jobRuntime) claim(run model.JobRun, legacyIO bool) bool {
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
		steps := []model.JobStep{{JobRunID: run.ID, Sequence: 2, Name: model.JobStepDispatch, Status: model.JobStatusSuccess,
			StartedAt: &dispatchStarted, FinishedAt: &dispatchFinished}}
		if legacyIO {
			steps = append(steps, model.JobStep{JobRunID: run.ID, Sequence: 3, Name: model.JobStepExecute,
				Status: model.JobStatusRunning, StartedAt: &dispatchFinished})
		}
		if err := tx.Create(&steps).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusRunning)
	})
	return err == nil
}

func (r *jobRuntime) beginPersist(run model.JobRun, resultCommittedByHandler bool) error {
	now := time.Now()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		var current model.JobRun
		if err := tx.Select("id", "status", "cancel_requested").First(&current, run.ID).Error; err != nil {
			return err
		}
		if current.Status != model.JobStatusRunning || (current.CancelRequested && !resultCommittedByHandler) {
			return errJobCancelWon
		}
		if err := tx.Model(&model.JobStep{}).
			Where("job_run_id = ? AND status = ?", run.ID, model.JobStatusRunning).
			Updates(map[string]any{"status": model.JobStatusSuccess, "finished_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		var maxSequence int
		if err := tx.Model(&model.JobStep{}).Where("job_run_id = ?", run.ID).
			Select("COALESCE(MAX(sequence), 0)").Scan(&maxSequence).Error; err != nil {
			return err
		}
		started := now
		if err := tx.Create(&model.JobStep{JobRunID: run.ID, Sequence: maxSequence + 1, Name: model.JobStepPersist,
			Status: model.JobStatusRunning, StartedAt: &started}).Error; err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "step", model.JobStatusRunning)
	})
}

func (r *jobRuntime) persistSuccess(run model.JobRun, binding durableJobBinding, result DurableJobResult, resultJSON []byte, latency int64) error {
	if err := r.beginPersist(run, binding.resultCommittedByHandler); err != nil {
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
		query := tx.Model(&model.JobRun{}).Where("id = ? AND status = ?", run.ID, model.JobStatusRunning)
		if binding.resultCommittedByHandler {
			// 原业务结果已先提交，成功是竞态中的既成事实；清除尚未收敛的
			// cancel_requested，保证 JobRun 与业务事实保持同一终态。
			updates["cancel_requested"] = false
		} else {
			query = query.Where("cancel_requested = ?", false)
		}
		res := query.Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errJobCancelWon
		}
		if binding.persistSuccess == nil {
			return errors.New("作业结果绑定不可用")
		}
		if err := binding.persistSuccess(tx, &run, result, resultJSON, now); err != nil {
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
	binding := r.bindingForRun(run)
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
		if err := binding.finishFailure(tx, &run, model.JobStatusFailed, code, message, now); err != nil {
			return err
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
	binding := r.bindingForRun(run)
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
		if err := binding.finishFailure(tx, &run, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
			return err
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
	binding := r.bindingForRun(run)
	_ = common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.JobRun{}).Where("id = ? AND status = ?", jobID, model.JobStatusQueued).
			Updates(map[string]any{
				"status": model.JobStatusFailed, "active_key": nil, "error": safeMessage,
				"error_code": code, "finished_at": now, "updated_at": now,
			})
		if res.Error != nil || res.RowsAffected != 1 {
			return res.Error
		}
		if err := binding.finishFailure(tx, &run, model.JobStatusFailed, code, message, now); err != nil {
			return err
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
	binding := r.bindingForRun(run)
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
		if err := binding.finishFailure(tx, &run, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
			return err
		}
		if err := finishRunningJobSteps(tx, run.ID, model.JobStatusCanceled, JobErrorCanceled, "作业已取消", now); err != nil {
			return err
		}
		return appendJobEvent(tx, run.UserID, run.ID, "status", model.JobStatusCanceled)
	})
}
