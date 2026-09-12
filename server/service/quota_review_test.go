package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func reviewCompareRows() []CompareRow {
	return []CompareRow{{Symbol: "600000", Market: "cn", QuoteOK: true, Price: 10}, {Symbol: "600001", Market: "cn", QuoteOK: true, Price: 20}}
}

func TestQuotaCountsManualActionWithoutTokenUsage(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "本地测试点评"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(upstream.Close)
	const userID int64 = 1110
	seedReportEnv(t, userID, upstream.URL)
	if err := common.DB.Create(&model.UserQuota{UserID: userID, ActionLimit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &CompareService{llm: NewLLMService()}
	comment, note, code, _, _ := svc.aiComment(t.Context(), userID, true, 0, reviewCompareRows())
	if code != "" || comment == "" {
		t.Fatalf("首次点评应成功：comment=%s code=%s note=%s", comment, code, note)
	}
	_, _, code, _, _ = svc.aiComment(t.Context(), userID, true, 0, reviewCompareRows())
	quota, err := getUserQuota(userID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || quota.ActionUsed != 1 || code != RefusalQuotaExhausted {
		t.Fatalf("次数额度不能依赖上游是否返回 usage：calls=%d quota=%+v code=%s", calls.Load(), quota, code)
	}
}

func TestQuotaRejectsConcurrentActionBeforeModelCall(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "本地并发点评"}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 10}})
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(unblock)
	const userID int64 = 1111
	seedReportEnv(t, userID, upstream.URL)
	if err := common.DB.Create(&model.UserQuota{UserID: userID, ActionLimit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	svc := &CompareService{llm: NewLLMService()}
	first, second := make(chan string, 1), make(chan string, 1)
	call := func(result chan<- string) {
		_, _, code, _, _ := svc.aiComment(ctx, userID, true, 0, reviewCompareRows())
		result <- code
	}
	go call(first)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("首个动作未到达本地模型")
	}
	go call(second)
	var secondCode string
	overrun := false
	select {
	case secondCode = <-second:
	case <-started:
		overrun = true
	case <-ctx.Done():
		t.Fatal("并发额度检查未结束")
	}
	unblock()
	if code := <-first; code != "" {
		t.Errorf("首个动作应完成：%s", code)
	}
	if overrun {
		secondCode = <-second
	}
	quota, err := getUserQuota(userID)
	if err != nil {
		t.Fatal(err)
	}
	if overrun || secondCode != RefusalQuotaExhausted || quota.ActionUsed != 1 {
		t.Fatalf("仅余一次额度时不能并发发出两次模型请求：overrun=%v code=%s quota=%+v", overrun, secondCode, quota)
	}
}

func TestUnusedQuotaRefundDoesNotCrossAdminReset(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1112
	if err := common.DB.Create(&model.User{ID: userID, Username: "quota-reset-review", Status: model.StatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.UserQuota{UserID: userID, ActionLimit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	_, refundCanceled, err := beginManualQuotaAction(canceled, userID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	refundCanceled()
	quota, err := getUserQuota(userID)
	if err != nil || quota.ActionUsed != 0 {
		t.Fatalf("取消且未取得模型响应时应释放额度：quota=%+v err=%v", quota, err)
	}
	_, refundOld, err := beginManualQuotaAction(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(refundOld)
	if _, err := NewAdminService().UpdateUserQuota(userID, QuotaUpdateInput{ActionLimit: 1, ResetUsed: true}); err != nil {
		t.Fatal(err)
	}
	current, finishCurrent, err := beginManualQuotaAction(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	noteManualQuotaResponse(current, &chatResult{Content: "重置后已经完成的动作"})
	finishCurrent()
	refundOld()
	quota, err = getUserQuota(userID)
	if err != nil || quota.ActionUsed != 1 {
		t.Fatalf("旧请求释放预留不能冲减管理员重置后的已用次数：quota=%+v err=%v", quota, err)
	}
}

func TestQuotaReservationAllowsNestedRecommendationAtLimit(t *testing.T) {
	const userID int64 = 1113
	seedReportEnv(t, userID, "http://127.0.0.1:1")
	if err := common.DB.Create(&model.UserQuota{UserID: userID, ActionLimit: 1}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := beginManualQuotaAction(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(finish)
	child := withoutJobExecution(ctx)
	if _, err := (&RecommendationService{llm: NewLLMService()}).prepareGeneration(userID, true, RecommendRequest{Type: model.RecTypeShortTerm}, false, child); err != nil {
		t.Fatalf("已预留的最后一次额度不能阻止日报自己的推荐子流程：%v", err)
	}
	_, childFinish, err := beginManualQuotaAction(child, userID)
	if err != nil {
		t.Fatal(err)
	}
	childFinish()
	if !errors.Is(checkQuota(userID), errQuotaExhausted) || checkQuota(userID, ctx) != nil {
		t.Fatal("外部新动作应被拒绝，同一动作的内部调用应可继续")
	}
	noteManualQuotaResponse(child, &chatResult{Content: "子流程响应"})
	finish()
	quota, err := getUserQuota(userID)
	if err != nil || quota.ActionUsed != 1 {
		t.Fatalf("嵌套调用与重复 finish 仍只计一次：quota=%+v err=%v", quota, err)
	}
}
