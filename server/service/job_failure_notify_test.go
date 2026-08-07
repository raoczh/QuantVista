package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

type recordingJobFailureNotifier struct {
	mu       sync.Mutex
	userIDs  []int64
	messages []NotifyMessage
	statuses []string
}

func (n *recordingJobFailureNotifier) SendMsgContext(_ context.Context, userID int64, msg NotifyMessage) {
	var run model.JobRun
	jobIDText := strings.TrimPrefix(msg.Route, "/tasks?job_id=")
	common.DB.Where("id = ?", jobIDText).First(&run)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.userIDs = append(n.userIDs, userID)
	n.messages = append(n.messages, msg)
	n.statuses = append(n.statuses, run.Status)
}

func TestJobFailureNotificationsIdempotentMergedMutedAuthorizedAndRedacted(t *testing.T) {
	setupTestDB(t)
	if !common.DB.Migrator().HasTable(&model.JobFailureNotification{}) {
		t.Fatal("JobFailureNotification 未进入统一自动迁移链路")
	}
	for _, table := range []string{"job_failure_notifications", "job_runs", "notify_channels", "user_preferences"} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, userID := range []int64{920001, 920002} {
		if err := common.DB.Create(&model.UserPreference{UserID: userID, EnableNotify: true}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.NotifyChannel{UserID: userID, Kind: model.NotifyKindWebhook, Name: "fake", Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	const mutedUserID int64 = 920003
	if err := common.DB.Create(&model.UserPreference{UserID: mutedUserID, EnableNotify: false}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.NotifyChannel{UserID: mutedUserID, Kind: model.NotifyKindWebhook, Name: "muted", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}

	createUserRun := func(userID int64, kind string) model.JobRun {
		run := model.JobRun{
			UserID: userID, OwnerType: model.JobOwnerUser, Kind: kind,
			RequestHash: strings.Repeat("a", 64), RequestSnapshot: `{"request":{"prompt":"完整模型正文不得进入通知"}}`,
			Status: model.JobStatusFailed, ErrorCode: "authorization=Bearer sk-secret",
			Error: "Bearer sk-secret；完整请求与模型正文", QueuedAt: time.Now(),
		}
		if err := common.DB.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		return run
	}
	run1 := createUserRun(920001, JobKindAnalysis)
	run2 := createUserRun(920001, JobKindAnalysis)
	run3 := createUserRun(920001, JobKindQA)
	otherRun := createUserRun(920002, JobKindAnalysis)
	mutedRun := createUserRun(mutedUserID, JobKindAnalysis)
	systemRun := model.JobRun{
		OwnerType: model.JobOwnerSystem, Kind: JobKindSyncDailyBars,
		RequestHash: strings.Repeat("b", 64), RequestSnapshot: `{}`, Status: model.JobStatusFailed,
		ErrorCode: "failed", Error: "system failed", QueuedAt: time.Now(),
	}
	if err := common.DB.Create(&systemRun).Error; err != nil {
		t.Fatal(err)
	}

	notifier := &recordingJobFailureNotifier{}
	runtime := newJobRuntime(1, 1)
	runtime.notifier = notifier
	runtime.notifyFailedJob(run1.ID)
	runtime.notifyFailedJob(run1.ID) // 同一 JobRun 重复观察
	runtime.notifyFailedJob(run2.ID) // 同类短窗合并
	runtime.notifyFailedJob(run3.ID) // 不同 kind 独立通知
	runtime.notifyFailedJob(otherRun.ID)
	runtime.notifyFailedJob(mutedRun.ID)
	runtime.notifyFailedJob(systemRun.ID)

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.messages) != 3 {
		t.Fatalf("同 JobRun/同类合并/静默/system 后应只外发 3 次，got %d", len(notifier.messages))
	}
	wantUsers := []int64{920001, 920001, 920002}
	for i, msg := range notifier.messages {
		if notifier.userIDs[i] != wantUsers[i] {
			t.Fatalf("通知收件人越权: got=%v want=%v", notifier.userIDs, wantUsers)
		}
		if notifier.statuses[i] != model.JobStatusFailed {
			t.Fatalf("通知必须在 failed 事务提交后发送: statuses=%v", notifier.statuses)
		}
		lower := strings.ToLower(msg.Content)
		for _, secret := range []string{"bearer", "sk-secret", "完整请求", "模型正文", "authorization"} {
			if strings.Contains(lower, strings.ToLower(secret)) {
				t.Fatalf("通知泄露敏感错误或请求正文 %q: %+v", secret, msg)
			}
		}
		if msg.Kind != NotifyMsgKindTaskFailure || !strings.HasPrefix(msg.Route, "/tasks?job_id=") {
			t.Fatalf("任务失败类别或精确深链错误: %+v", msg)
		}
	}

	var notices []model.JobFailureNotification
	if err := common.DB.Order("id").Find(&notices).Error; err != nil {
		t.Fatal(err)
	}
	if len(notices) != 5 {
		t.Fatalf("user failed 每个 JobRun 应有且只有一条投递事实，system 无记录: %+v", notices)
	}
	byJob := make(map[int64]model.JobFailureNotification, len(notices))
	for _, notice := range notices {
		byJob[notice.JobRunID] = notice
	}
	if byJob[run1.ID].Status != model.JobFailureNoticeDispatched || byJob[run1.ID].MergeCount != 2 {
		t.Fatalf("首条通知应记录短窗合并计数: %+v", byJob[run1.ID])
	}
	if byJob[run2.ID].Status != model.JobFailureNoticeMerged || byJob[run2.ID].MergeRootID == nil || *byJob[run2.ID].MergeRootID != byJob[run1.ID].ID {
		t.Fatalf("第二条同类失败应合并到首条: %+v", byJob[run2.ID])
	}
	if byJob[mutedRun.ID].Status != model.JobFailureNoticeSuppressed {
		t.Fatalf("关闭通知时应静默且记录幂等事实: %+v", byJob[mutedRun.ID])
	}
	if _, ok := byJob[systemRun.ID]; ok {
		t.Fatal("system JobRun 不得生成用户通知事实")
	}
	ownRows, err := NewTaskCenterService().List(920001, model.RoleUser, TaskCenterListOptions{JobID: run1.ID, IncludeSteps: true})
	if err != nil || len(ownRows) != 1 || ownRows[0].SourceID != run1.ID {
		t.Fatalf("任务深链必须精确返回本人 JobRun: rows=%+v err=%v", ownRows, err)
	}
	otherRows, err := NewTaskCenterService().List(920002, model.RoleUser, TaskCenterListOptions{JobID: run1.ID})
	if err != nil || len(otherRows) != 0 {
		t.Fatalf("任务深链必须由后端按用户隔离: rows=%+v err=%v", otherRows, err)
	}
}

func TestJobFailureNoticeMergeWindowCrossesBucketBoundary(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM job_failure_notifications")
	common.DB.Exec("DELETE FROM job_runs")
	const userID int64 = 920010
	createRun := func(hashChar string) model.JobRun {
		run := model.JobRun{
			UserID: userID, OwnerType: model.JobOwnerUser, Kind: JobKindAnalysis,
			RequestHash: strings.Repeat(hashChar, 64), RequestSnapshot: `{}`,
			Status: model.JobStatusFailed, ErrorCode: "failed", QueuedAt: time.Now(),
		}
		if err := common.DB.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		return run
	}
	run1, run2 := createRun("c"), createRun("d")
	bucketSeconds := int64(jobFailureMergeWindow / time.Second)
	boundary := time.Unix((time.Now().Unix()/bucketSeconds+1)*bucketSeconds, 0).In(time.Local)
	firstAt := boundary.Add(-time.Second)
	first, err := reserveJobFailureNotice(run1, true, firstAt)
	if err != nil || !first.Send {
		t.Fatalf("首条应声明外发: claim=%+v err=%v", first, err)
	}
	second, err := reserveJobFailureNotice(run2, true, boundary.Add(time.Second))
	if err != nil || second.Send || second.Notice.Status != model.JobFailureNoticeMerged || second.Notice.MergeRootID == nil {
		t.Fatalf("跨固定桶边界但相隔 2 秒仍应合并: claim=%+v err=%v", second, err)
	}
}
