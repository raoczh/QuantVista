package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func resetDurableJobs(t *testing.T) {
	t.Helper()
	resetAsyncLLMTasks(t)
}

func jobRunIDForTask(t *testing.T, taskID int64) int64 {
	t.Helper()
	var task model.LLMTask
	if err := common.DB.Select("job_run_id").First(&task, taskID).Error; err != nil {
		t.Fatal(err)
	}
	if task.JobRunID == nil {
		t.Fatalf("兼容任务 %d 缺少 job_run_id", taskID)
	}
	return *task.JobRunID
}

func waitJobRun(t *testing.T, userID, jobID int64) *JobRunView {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		view, err := GetJobRun(userID, jobID, true)
		if err == nil && view.Status != model.JobStatusQueued && view.Status != model.JobStatusRunning {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := GetJobRun(userID, jobID, true)
	t.Fatalf("等待作业终态超时: view=%+v err=%v", view, err)
	return nil
}

func waitJobStatus(t *testing.T, userID, jobID int64, status string) *JobRunView {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		view, err := GetJobRun(userID, jobID, true)
		if err == nil && view.Status == status {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待作业状态 %s 超时", status)
	return nil
}

func TestJobRuntimeCapacityAndInFlightDedup(t *testing.T) {
	resetDurableJobs(t)
	runtime := newJobRuntime(1, 1)
	defer runtime.close()

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	handler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return DurableJobResult{Value: map[string]any{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	first, err := runtime.start(801, JobKindCompare, map[string]any{"symbols": []string{"a", "b"}}, false, nil, &handler)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	duplicate, err := runtime.start(801, JobKindCompare, map[string]any{"symbols": []string{"a", "b"}}, false, nil, &handler)
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("同用户同 kind/hash 应复用在途作业: first=%+v duplicate=%+v err=%v", first, duplicate, err)
	}
	if _, err := runtime.start(801, JobKindCompare, map[string]any{"symbols": []string{"c", "d"}}, false, nil, &handler); !errors.Is(err, ErrJobQueueBusy) {
		t.Fatalf("满载应返回稳定 busy 错误: %v", err)
	}
	var coded interface{ RefusalCode() string }
	if !errors.As(ErrJobQueueBusy, &coded) || coded.RefusalCode() != JobErrorBusy {
		t.Fatalf("busy 机读码不稳定: %v", ErrJobQueueBusy)
	}
	var count int64
	if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", 801).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("busy/幂等不得多建事实行: count=%d err=%v", count, err)
	}
	var active model.JobRun
	if err := common.DB.Where("user_id = ? AND status = ?", 801, model.JobStatusRunning).First(&active).Error; err != nil {
		t.Fatal(err)
	}
	duplicateRun := model.JobRun{UserID: 801, Kind: active.Kind, RequestHash: strings.Repeat("f", 64),
		ActiveKey: active.ActiveKey, Status: model.JobStatusQueued, SnapshotVersion: jobSnapshotVersion,
		RequestSnapshot: active.RequestSnapshot, QueuedAt: time.Now()}
	if err := common.DB.Create(&duplicateRun).Error; err == nil {
		t.Fatal("数据库唯一 active_key 必须拒绝跨实例式重复在途行")
	}
	close(release)
	waitJobRun(t, 801, jobRunIDForTask(t, first.ID))
}

func TestJobRuntimeCancelQueuedAndRunningRace(t *testing.T) {
	resetDurableJobs(t)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	var secondCalls atomic.Int32
	firstHandler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		close(firstStarted)
		<-firstRelease
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	secondHandler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		secondCalls.Add(1)
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	first, err := runtime.start(802, JobKindCompare, map[string]int{"n": 1}, false, nil, &firstHandler)
	if err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	second, err := runtime.start(802, JobKindCompare, map[string]int{"n": 2}, false, nil, &secondHandler)
	if err != nil {
		t.Fatal(err)
	}
	secondJobID := jobRunIDForTask(t, second.ID)
	if _, err := CancelJobRun(802, secondJobID); err != nil {
		t.Fatal(err)
	}
	if got := waitJobRun(t, 802, secondJobID); got.Status != model.JobStatusCanceled || secondCalls.Load() != 0 {
		t.Fatalf("queued 取消后不得执行: run=%+v calls=%d", got, secondCalls.Load())
	}
	close(firstRelease)
	waitJobRun(t, 802, jobRunIDForTask(t, first.ID))

	cancelStarted := make(chan struct{})
	cancelHandler := durableJobHandler{timeout: time.Minute, run: func(ctx context.Context, _ int64, _ bool, _ json.RawMessage) (DurableJobResult, error) {
		close(cancelStarted)
		<-ctx.Done()
		// 故意在取消后返回结果，验证 cancel_requested 与成功 CAS 只能有一个获胜。
		return DurableJobResult{Value: map[string]string{"late": "result"}, Status: model.JobStatusSuccess}, nil
	}}
	runningTask, err := runtime.start(802, JobKindQA, map[string]string{"question": "cancel"}, false, nil, &cancelHandler)
	if err != nil {
		t.Fatal(err)
	}
	<-cancelStarted
	runningJobID := jobRunIDForTask(t, runningTask.ID)
	if _, err := CancelJobRun(999, runningJobID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("其他用户不得取消作业: %v", err)
	}
	if _, err := CancelJobRun(802, runningJobID); err != nil {
		t.Fatal(err)
	}
	done := waitJobRun(t, 802, runningJobID)
	if done.Status != model.JobStatusCanceled {
		t.Fatalf("取消应赢得唯一终态: %+v", done)
	}
	legacy, err := GetAsyncLLMTask(802, runningTask.ID)
	if err != nil || legacy.Status != model.LLMTaskStatusFailed || len(legacy.Result) != 0 {
		t.Fatalf("取消不得持久化迟到结果: task=%+v err=%v", legacy, err)
	}
}

func TestJobTerminalCASAndUserIsolation(t *testing.T) {
	resetDurableJobs(t)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	handler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	task, err := runtime.start(803, JobKindScreenerParse, map[string]string{"text": "x"}, false, nil, &handler)
	if err != nil {
		t.Fatal(err)
	}
	jobID := jobRunIDForTask(t, task.ID)
	done := waitJobRun(t, 803, jobID)
	if done.Status != model.JobStatusSuccess {
		t.Fatalf("预期成功: %+v", done)
	}
	var persisted model.JobRun
	if err := common.DB.First(&persisted, jobID).Error; err != nil {
		t.Fatal(err)
	}
	runtime.finishFailed(persisted, "late_failure", "迟到失败")
	after, err := GetJobRun(803, jobID, true)
	if err != nil || after.Status != model.JobStatusSuccess || after.ErrorCode != "" {
		t.Fatalf("终态不得回退: run=%+v err=%v", after, err)
	}
	if _, err := GetJobRun(804, jobID, true); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("其他用户不得读取作业: %v", err)
	}
	if _, err := RetryJobRun(804, jobID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("其他用户不得重跑作业: %v", err)
	}
	invalid := model.JobRun{UserID: 803, Kind: "bad", RequestHash: strings.Repeat("a", 64),
		Status: "processing", RequestSnapshot: `{}`, QueuedAt: time.Now()}
	if err := common.DB.Create(&invalid).Error; err == nil {
		t.Fatal("数据库 CHECK 应拒绝状态枚举外的值")
	}
}

func TestJobRetryCreatesChildAndKeepsParent(t *testing.T) {
	resetDurableJobs(t)
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	secondRelease := make(chan struct{})
	RegisterDurableLLMJobHandler(JobKindQA, time.Minute,
		func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
			switch calls.Add(1) {
			case 1:
				return DurableJobResult{}, errors.New("first failed")
			case 2:
				close(secondStarted)
				<-secondRelease
			}
			return DurableJobResult{Value: QaTaskResult{ConversationID: 99}, Status: model.JobStatusSuccess}, nil
		})
	parentTask, err := StartDurableLLMTask(805, JobKindQA, map[string]string{"question": "retry"}, false)
	if err != nil {
		t.Fatal(err)
	}
	parentID := jobRunIDForTask(t, parentTask.ID)
	if got := waitJobRun(t, 805, parentID); got.Status != model.JobStatusFailed {
		t.Fatalf("父作业应失败: %+v", got)
	}
	activeTask, err := StartDurableLLMTask(805, JobKindQA, map[string]string{"question": "retry"}, false)
	if err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	if _, err := RetryJobRun(805, parentID); !errors.Is(err, ErrJobAlreadyRunning) {
		t.Fatalf("同请求已有在途任务时，重跑必须明确拒绝而非返回无 parent_id 的任务: %v", err)
	}
	var coded interface{ RefusalCode() string }
	if !errors.As(ErrJobAlreadyRunning, &coded) || coded.RefusalCode() != JobErrorAlreadyRunning {
		t.Fatalf("在途重跑拒绝码不稳定: %v", ErrJobAlreadyRunning)
	}
	var rejectedChildren int64
	if err := common.DB.Model(&model.JobRun{}).Where("parent_id = ?", parentID).Count(&rejectedChildren).Error; err != nil || rejectedChildren != 0 {
		t.Fatalf("被拒绝的重跑不得创建子作业: count=%d err=%v", rejectedChildren, err)
	}
	close(secondRelease)
	waitJobRun(t, 805, jobRunIDForTask(t, activeTask.ID))

	child, err := RetryJobRun(805, parentID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID == parentID || child.ParentID == nil || *child.ParentID != parentID {
		t.Fatalf("重跑必须创建带 parent_id 的新 run: %+v", child)
	}
	if got := waitJobRun(t, 805, child.ID); got.Status != model.JobStatusSuccess {
		t.Fatalf("子作业应成功: %+v", got)
	}
	parent, err := GetJobRun(805, parentID, false)
	if err != nil || parent.Status != model.JobStatusFailed || parent.ParentID != nil {
		t.Fatalf("重跑不得修改父作业: parent=%+v err=%v", parent, err)
	}

	unsupportedSnapshot, hash, err := makeJobSnapshot("unsupported", map[string]int{"x": 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := model.JobRun{UserID: 805, Kind: "unsupported", RequestHash: hash,
		Status: model.JobStatusFailed, RequestSnapshot: string(unsupportedSnapshot), SnapshotVersion: jobSnapshotVersion,
		QueuedAt: time.Now()}
	if err := common.DB.Create(&unsupported).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RetryJobRun(805, unsupported.ID); !errors.Is(err, ErrJobKindUnsupported) {
		t.Fatalf("不支持的 kind 必须明确拒绝: %v", err)
	}
}

func createPersistedJobForRecovery(t *testing.T, userID int64, kind, status string, request any) model.JobRun {
	t.Helper()
	snapshot, hash, err := makeJobSnapshot(kind, request, false)
	if err != nil {
		t.Fatal(err)
	}
	active := activeJobKey(userID, kind, hash+status)
	// 同 kind/request 的恢复样本需要不同请求哈希，避免测试数据本身违反在途唯一。
	hashSum := strings.Repeat("0", 64)
	if encoded, _, err := makeJobSnapshot(kind, map[string]any{"request": request, "status": status}, false); err == nil {
		snapshot = encoded
		sum := activeJobKey(userID, kind, string(encoded))
		hashSum = sum
		active = activeJobKey(userID, kind, hashSum)
	}
	now := time.Now().Add(-time.Minute)
	run := model.JobRun{UserID: userID, Kind: kind, RequestHash: hashSum, ActiveKey: &active,
		Status: status, SnapshotVersion: jobSnapshotVersion, RequestSnapshot: string(snapshot),
		ResultType: "llm_task", QueuedAt: now, CreatedAt: now, UpdatedAt: now}
	if status == model.JobStatusRunning {
		run.StartedAt = &now
	}
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		task := model.LLMTask{UserID: userID, Kind: kind, JobRunID: &run.ID,
			RequestHash: run.RequestHash, Status: model.LLMTaskStatusProcessing}
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		run.ResultID = &task.ID
		if err := tx.Model(&model.JobRun{}).Where("id = ?", run.ID).Update("result_id", task.ID).Error; err != nil {
			return err
		}
		stepName := model.JobStepQueued
		sequence := 1
		if status == model.JobStatusRunning {
			stepName = model.JobStepExecute
			sequence = 3
		}
		return tx.Create(&model.JobStep{JobRunID: run.ID, Sequence: sequence, Name: stepName,
			Status: model.JobStatusRunning, StartedAt: &now}).Error
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestJobRestartRecoveryConvergesRunningAndResumesQueued(t *testing.T) {
	resetDurableJobs(t)
	running := createPersistedJobForRecovery(t, 806, JobKindCompare, model.JobStatusRunning, map[string]int{"n": 1})
	queued := createPersistedJobForRecovery(t, 806, JobKindQA, model.JobStatusQueued, map[string]string{"question": "recover"})

	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	runtime.register(JobKindQA, time.Minute,
		func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
			return DurableJobResult{Value: QaTaskResult{ConversationID: 7}, Status: model.JobStatusSuccess}, nil
		})
	runtime.startWorkers()
	if err := runtime.recoverPersisted(); err != nil {
		t.Fatal(err)
	}
	if got := waitJobRun(t, 806, running.ID); got.Status != model.JobStatusFailed || got.ErrorCode != JobErrorInterrupted {
		t.Fatalf("重启后 running 必须明确失败收敛: %+v", got)
	}
	if got := waitJobRun(t, 806, queued.ID); got.Status != model.JobStatusSuccess {
		t.Fatalf("重启后 queued 应按规则恢复: %+v", got)
	}
}

func TestJobSnapshotBoundAndSensitiveFields(t *testing.T) {
	resetDurableJobs(t)
	if _, _, err := makeJobSnapshot(JobKindQA, map[string]string{"api_key": "sk-secret"}, false); !errors.Is(err, ErrJobSnapshotSensitive) {
		t.Fatalf("API Key 字段必须拒绝持久化: %v", err)
	}
	if _, _, err := makeJobSnapshot(JobKindQA, map[string]string{"system_prompt": "secret"}, false); !errors.Is(err, ErrJobSnapshotSensitive) {
		t.Fatalf("系统 prompt 字段必须拒绝持久化: %v", err)
	}
	if _, _, err := makeJobSnapshot(JobKindQA, map[string]string{"question": strings.Repeat("x", jobSnapshotMaxBytes)}, false); !errors.Is(err, ErrJobSnapshotTooLarge) {
		t.Fatalf("超限快照必须拒绝: %v", err)
	}
	snapshot, _, err := makeJobSnapshot(JobKindQA, map[string]any{"question": "safe", "llm_config_id": 3}, true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(snapshot)
	for _, forbidden := range []string{"sk-secret", "system_prompt", "result_json", "messages", "allow_private"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("快照泄露禁止内容 %q: %s", forbidden, text)
		}
	}
	encoded, err := json.Marshal(model.JobRun{UserID: 1, RequestHash: "secret-hash", RequestSnapshot: text})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-hash") || strings.Contains(string(encoded), "question") || strings.Contains(string(encoded), "user_id") {
		t.Fatalf("JobRun 默认 JSON 不得暴露用户或请求快照: %s", encoded)
	}

	runtime := newJobRuntime(1, 1)
	defer runtime.close()
	handler := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		return DurableJobResult{}, errors.New(`upstream authorization: Bearer sk-secret api_key=also-secret`)
	}}
	task, err := runtime.start(903, JobKindQA, map[string]string{"question": "safe"}, false, nil, &handler)
	if err != nil {
		t.Fatal(err)
	}
	run := waitJobRun(t, 903, jobRunIDForTask(t, task.ID))
	legacy, err := GetAsyncLLMTask(903, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(struct {
		Run    *JobRunView  `json:"run"`
		Legacy *LLMTaskView `json:"legacy"`
	}{Run: run, Legacy: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "sk-secret") || strings.Contains(string(persisted), "also-secret") {
		t.Fatalf("作业错误与步骤不得持久化凭据: %s", persisted)
	}
}

func TestJobExecutionRechecksEnabledAdminAndIgnoresLegacyPermission(t *testing.T) {
	resetDurableJobs(t)
	const userID int64 = 904
	common.DB.Where("id = ?", userID).Delete(&model.User{})
	if err := common.DB.Create(&model.User{
		ID: userID, Username: "job-admin-904", Role: model.RoleAdmin, Status: model.StatusEnabled,
	}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Where("id = ?", userID).Delete(&model.User{}) })

	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	blockerStarted := make(chan struct{})
	blockerRelease := make(chan struct{})
	blocker := durableJobHandler{timeout: time.Minute, run: func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
		close(blockerStarted)
		<-blockerRelease
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	first, err := runtime.start(905, JobKindCompare, map[string]int{"n": 1}, false, nil, &blocker)
	if err != nil {
		t.Fatal(err)
	}
	<-blockerStarted

	allowSeen := make(chan bool, 1)
	queuedHandler := durableJobHandler{timeout: time.Minute, run: func(_ context.Context, _ int64, allowPrivate bool, _ json.RawMessage) (DurableJobResult, error) {
		allowSeen <- allowPrivate
		return DurableJobResult{Value: map[string]bool{"ok": true}, Status: model.JobStatusSuccess}, nil
	}}
	queuedTask, err := runtime.start(userID, JobKindQA, map[string]string{"question": "permission"}, true, nil, &queuedHandler)
	if err != nil {
		t.Fatal(err)
	}
	queuedJobID := jobRunIDForTask(t, queuedTask.ID)
	var queuedRun model.JobRun
	if err := common.DB.First(&queuedRun, queuedJobID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(queuedRun.RequestSnapshot, "allow_private") {
		t.Fatalf("新快照不得持久化提交时权限: %s", queuedRun.RequestSnapshot)
	}
	// 模拟升级前快照：旧字段仍可解码，但不得再成为授权依据。
	var legacy map[string]any
	if err := json.Unmarshal([]byte(queuedRun.RequestSnapshot), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["allow_private"] = true
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.JobRun{}).Where("id = ?", queuedJobID).Update("request_snapshot", string(legacyJSON)).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.User{}).Where("id = ?", userID).Update("status", model.StatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	close(blockerRelease)
	if allow := <-allowSeen; allow {
		t.Fatal("执行时账号已禁用，旧快照中的 allow_private 不得继续授权")
	}
	waitJobRun(t, 905, jobRunIDForTask(t, first.ID))
	if got := waitJobRun(t, userID, queuedJobID); got.Status != model.JobStatusSuccess {
		t.Fatalf("旧快照字段应兼容读取，作业本身仍可按当前权限执行: %+v", got)
	}
}

func TestJobEventsAreMonotonicAndUserIsolated(t *testing.T) {
	resetDurableJobs(t)
	for _, event := range []model.JobEvent{
		{UserID: 901, JobRunID: 1, Type: "created", Status: model.JobStatusQueued},
		{UserID: 902, JobRunID: 2, Type: "created", Status: model.JobStatusQueued},
		{UserID: 901, JobRunID: 1, Type: "status", Status: model.JobStatusRunning},
	} {
		if err := common.DB.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	all, err := ListJobEvents(901, 0, 100)
	if err != nil || len(all) != 2 || all[0].ID >= all[1].ID {
		t.Fatalf("本人事件必须单调且不混入其他用户: events=%+v err=%v", all, err)
	}
	resumed, err := ListJobEvents(901, all[0].ID, 100)
	if err != nil || len(resumed) != 1 || resumed[0].ID != all[1].ID {
		t.Fatalf("事件续传游标错误: events=%+v err=%v", resumed, err)
	}
	other, err := ListJobEvents(902, 0, 100)
	if err != nil || len(other) != 1 || other[0].JobRunID != 2 {
		t.Fatalf("SSE 事件用户隔离失败: events=%+v err=%v", other, err)
	}
}
