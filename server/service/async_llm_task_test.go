package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func resetAsyncLLMTasks(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM llm_tasks").Error; err != nil {
		t.Fatalf("清理任务表失败: %v", err)
	}
}

func waitAsyncLLMTask(t *testing.T, userID, id int64) *LLMTaskView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := GetAsyncLLMTask(userID, id)
		if err == nil && view.Status != model.LLMTaskStatusProcessing {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务 %d 超时未完成", id)
	return nil
}

func TestAsyncLLMTaskSuccessAndDedup(t *testing.T) {
	resetAsyncLLMTasks(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	runner := func(context.Context) (any, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return map[string]any{"answer": "ok", "items": []int{1, 2}}, nil
	}

	first, err := StartAsyncLLMTask(101, "compare", map[string]any{"b": 2, "a": 1}, time.Minute, runner)
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	second, err := StartAsyncLLMTask(101, "compare", map[string]any{"a": 1, "b": 2}, time.Minute, runner)
	if err != nil {
		t.Fatalf("复用任务失败: %v", err)
	}
	if first.ID == 0 || second.ID != first.ID || first.Status != model.LLMTaskStatusProcessing {
		t.Fatalf("同请求在途任务应复用: first=%+v second=%+v", first, second)
	}
	<-entered
	if calls.Load() != 1 {
		t.Fatalf("runner 只应启动一次: %d", calls.Load())
	}
	close(release)

	done := waitAsyncLLMTask(t, 101, first.ID)
	if done.Status != model.LLMTaskStatusSuccess || !json.Valid(done.Result) ||
		!strings.Contains(string(done.Result), `"answer":"ok"`) {
		t.Fatalf("成功结果不正确: %+v result=%s", done, done.Result)
	}
	done.Result[0] = '['
	again, err := GetAsyncLLMTask(101, first.ID)
	if err != nil || !json.Valid(again.Result) || again.Result[0] != '{' {
		t.Fatalf("调用方修改 view 不应污染持久化结果: result=%s err=%v", again.Result, err)
	}
	rows, err := ListAsyncLLMTasks(101, "compare", model.LLMTaskStatusSuccess, 10)
	if err != nil || len(rows) != 1 || rows[0].Result != nil {
		t.Fatalf("列表应返回摘要且不加载 result: rows=%+v err=%v", rows, err)
	}
}

func TestAsyncLLMTaskFailurePanicAndTimeout(t *testing.T) {
	resetAsyncLLMTasks(t)

	failed, err := StartAsyncLLMTask(102, "analysis", "failed", time.Minute, func(context.Context) (any, error) {
		return nil, &RefusalError{Code: RefusalLLMOutputInvalid, Msg: "输出不合法"}
	})
	if err != nil {
		t.Fatal(err)
	}
	failedDone := waitAsyncLLMTask(t, 102, failed.ID)
	if failedDone.Status != model.LLMTaskStatusFailed || failedDone.ErrorCode != RefusalLLMOutputInvalid {
		t.Fatalf("业务错误码未保留: %+v", failedDone)
	}

	panicked, err := StartAsyncLLMTask(102, "analysis", "panic", time.Minute, func(context.Context) (any, error) {
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	panicDone := waitAsyncLLMTask(t, 102, panicked.ID)
	if panicDone.ErrorCode != AsyncLLMTaskErrorPanic || !strings.Contains(panicDone.Error, "boom") {
		t.Fatalf("panic 未可靠收尾: %+v", panicDone)
	}

	timed, err := StartAsyncLLMTask(102, "analysis", "timeout", 20*time.Millisecond, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	timeoutDone := waitAsyncLLMTask(t, 102, timed.ID)
	if timeoutDone.ErrorCode != AsyncLLMTaskErrorTimeout {
		t.Fatalf("deadline 应归为 task_timeout: %+v", timeoutDone)
	}
}

func TestAsyncLLMTaskStaleIsolationAndSnapshot(t *testing.T) {
	resetAsyncLLMTasks(t)
	stale := model.LLMTask{UserID: 103, Kind: "qa", RequestHash: strings.Repeat("a", 64), Status: model.LLMTaskStatusProcessing}
	if err := common.DB.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	common.DB.Model(&model.LLMTask{}).Where("id = ?", stale.ID).
		Update("updated_at", time.Now().Add(-16*time.Minute))
	if _, err := GetAsyncLLMTask(104, stale.ID); err == nil {
		t.Fatal("其他用户不应读取任务")
	}
	view, err := GetAsyncLLMTask(103, stale.ID)
	if err != nil || view.Status != model.LLMTaskStatusFailed || view.ErrorCode != AsyncLLMTaskErrorStale {
		t.Fatalf("stale 应惰性失败: view=%+v err=%v", view, err)
	}
	otherRows, err := ListAsyncLLMTasks(104, "", "", 10)
	if err != nil || len(otherRows) != 0 {
		t.Fatalf("其他用户列表不应出现任务: rows=%+v err=%v", otherRows, err)
	}
	ownerRows, err := ListAsyncLLMTasks(103, "qa", model.LLMTaskStatusFailed, 10)
	if err != nil || len(ownerRows) != 1 || ownerRows[0].ID != stale.ID {
		t.Fatalf("本人列表应能按 kind/status 查询任务: rows=%+v err=%v", ownerRows, err)
	}

	req := map[string]string{"value": "before"}
	seen := make(chan string, 1)
	snapshotTask, err := StartAsyncLLMTaskSnapshot(103, "snapshot", req, time.Minute,
		func(_ context.Context, raw json.RawMessage) (any, error) {
			var got map[string]string
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			seen <- got["value"]
			return got, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	req["value"] = "after"
	if got := <-seen; got != "before" {
		t.Fatalf("后台请求快照被调用方修改污染: %q", got)
	}
	if done := waitAsyncLLMTask(t, 103, snapshotTask.ID); done.Status != model.LLMTaskStatusSuccess {
		t.Fatalf("快照任务失败: %+v", done)
	}
}

func TestAsyncLLMTaskRejectsInvalidInputsAndResult(t *testing.T) {
	resetAsyncLLMTasks(t)
	if _, err := StartAsyncLLMTask(1, "", nil, time.Minute, func(context.Context) (any, error) { return nil, nil }); err == nil {
		t.Fatal("空 kind 应拒绝")
	}
	if _, err := StartAsyncLLMTask(1, "x", make(chan int), time.Minute, func(context.Context) (any, error) { return nil, nil }); err == nil {
		t.Fatal("不可序列化请求应拒绝")
	}
	task, err := StartAsyncLLMTask(1, "x", "bad-result", time.Minute, func(context.Context) (any, error) {
		return make(chan int), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := waitAsyncLLMTask(t, 1, task.ID)
	if done.ErrorCode != AsyncLLMTaskErrorResultEncode {
		t.Fatalf("不可序列化结果应失败: %+v", done)
	}

	stored := model.LLMTask{UserID: 9, RequestHash: "secret-hash", ResultJSON: `{"secret":true}`}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "secret-hash") || strings.Contains(text, "secret") || strings.Contains(text, "user_id") {
		t.Fatalf("模型默认 JSON 不得暴露用户、请求哈希或结果大字段: %s", text)
	}
}
