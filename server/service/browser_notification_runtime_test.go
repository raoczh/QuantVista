package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestBrowserPushRetryAndExpiredQuoteSuppression(t *testing.T) {
	const userID = int64(71110)
	cleanBrowserNotificationTables(t, userID)
	setValidVAPIDEnv(t)
	now := time.Now().UTC()
	fake := &fakeBrowserPushSender{results: map[string]fakeBrowserPushResult{"https://push.example.com/retry": {http.StatusServiceUnavailable, errors.New("暂不可用")}}}
	svc := &BrowserNotificationService{sender: fake, now: func() time.Time { return now }}
	device, err := svc.UpsertSubscription(userID, validBrowserSubscription("retry-browser-device-001", "回归设备", "https://push.example.com/retry"))
	if err != nil {
		t.Fatal(err)
	}
	event, err := svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{SourceType: "alert", SourceID: 1,
		FactKey: "retry-fact", Category: model.BrowserNotifyCategoryAlert, Level: "review", Title: "到价"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("首次投递次数=%d", len(fake.calls))
	}
	if err := svc.RetryPendingPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatal("退避时间之前不得重发")
	}
	now = now.Add(16 * time.Second)
	fake.results = nil
	if err := svc.RetryPendingPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 2 {
		t.Fatal("临时失败应自动恢复")
	}
	if err := svc.RetryPendingPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 2 {
		t.Fatal("推送服务已经接收的事件不得重复重试")
	}
	var delivery model.BrowserNotificationDelivery
	common.DB.Where("event_id = ? AND device_id = ?", event.ID, device.ID).First(&delivery)
	if delivery.AttemptCount != 2 || delivery.PushDeliveredAt == nil || delivery.ForegroundAckAt != nil || delivery.PushLeaseToken != "" {
		t.Fatalf("服务接收与页面展示必须区分：%+v", delivery)
	}
	// 模拟创建后进程退出，另一服务实例应能承接未发送的持久记录。
	svc.sender = nil
	_, err = svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{SourceType: "alert", SourceID: 2,
		FactKey: "restart-fact", Category: model.BrowserNotifyCategoryAlert, Title: "重启前到价"}, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted := &BrowserNotificationService{sender: fake, now: func() time.Time { return now }}
	if err := restarted.RetryPendingPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 3 {
		t.Fatal("未投递事件未在重启后恢复")
	}
	_, err = svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{SourceType: "alert", SourceID: 3,
		FactKey: "old-fact", Category: model.BrowserNotifyCategoryAlert, Title: "旧行情"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	if err := restarted.RetryPendingPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 3 {
		t.Fatal("过期行情不应补弹成当前买卖提醒")
	}
	rows, err := svc.PendingEvents(userID, "retry-browser-device-001", 0, 20)
	if err != nil || len(rows) != 0 {
		t.Fatalf("在线入口也必须屏蔽过期提醒 rows=%v err=%v", rows, err)
	}
}

func TestBrowserLongPollWakesAndCancels(t *testing.T) {
	const userID = int64(71111)
	cleanBrowserNotificationTables(t, userID)
	svc := &BrowserNotificationService{now: time.Now}
	_, err := svc.UpsertSubscription(userID, BrowserSubscriptionInput{DeviceKey: "long-poll-browser-0001", Name: "在线"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		rows []BrowserEventView
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		rows, err := svc.WaitPendingEvents(ctx, userID, "long-poll-browser-0001", 0, 20, 25)
		resultCh <- result{rows, err}
	}()
	_, err = svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{SourceType: "alert", SourceID: 1,
		FactKey: "wake-fact", Category: model.BrowserNotifyCategoryAlert, Title: "买入区间触达"}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := <-resultCh
	if got.err != nil || len(got.rows) != 1 {
		t.Fatalf("事件应立即唤醒长轮询：%+v", got)
	}
	if err := svc.Ack(userID, got.rows[0].DeliveryID, "long-poll-browser-0001"); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if _, err := svc.WaitPendingEvents(canceled, userID, "long-poll-browser-0001", 0, 20, 25); err == nil {
		t.Fatal("断开的请求必须退出")
	}
}
