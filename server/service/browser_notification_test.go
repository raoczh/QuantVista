package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type fakeBrowserPushResult struct {
	status int
	err    error
}

type fakeBrowserPushSender struct {
	results map[string]fakeBrowserPushResult
	calls   []browserPushPlainSubscription
	payload []map[string]any
}

func (f *fakeBrowserPushSender) Send(_ context.Context, payload []byte, sub browserPushPlainSubscription) (int, error) {
	f.calls = append(f.calls, sub)
	var decoded map[string]any
	_ = json.Unmarshal(payload, &decoded)
	f.payload = append(f.payload, decoded)
	result, ok := f.results[sub.Endpoint]
	if !ok {
		return http.StatusCreated, nil
	}
	return result.status, result.err
}

func setValidVAPIDEnv(t *testing.T) {
	t.Helper()
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("生成测试 VAPID 密钥失败: %v", err)
	}
	t.Setenv("VAPID_PUBLIC_KEY", publicKey)
	t.Setenv("VAPID_PRIVATE_KEY", privateKey)
	t.Setenv("VAPID_SUBJECT", "mailto:ops@example.com")
}

func validBrowserSubscription(deviceKey, name, endpoint string) BrowserSubscriptionInput {
	key := make([]byte, 65)
	key[0] = 4
	for i := 1; i < len(key); i++ {
		key[i] = byte(66 + i%40)
	}
	auth := []byte("0123456789abcdef")
	return BrowserSubscriptionInput{
		DeviceKey: deviceKey,
		Name:      name,
		Endpoint:  endpoint,
		P256dh:    base64.RawURLEncoding.EncodeToString(key),
		Auth:      base64.RawURLEncoding.EncodeToString(auth),
	}
}

func cleanBrowserNotificationTables(t *testing.T, userIDs ...int64) {
	t.Helper()
	setupTestDB(t)
	if len(userIDs) == 0 {
		userIDs = []int64{71001, 71002, 71003}
	}
	for _, table := range []string{
		"browser_notification_deliveries", "browser_notification_events", "web_push_subscriptions",
		"browser_notification_devices", "browser_notification_preferences", "notify_channels", "user_preferences",
	} {
		if err := common.DB.Exec("DELETE FROM "+table+" WHERE user_id IN ?", userIDs).Error; err != nil {
			t.Fatalf("清理 %s 失败: %v", table, err)
		}
	}
	common.EncryptionKey = "test-encryption-key-1234567890"
}

func TestBrowserSubscriptionLifecycleAndUserIsolation(t *testing.T) {
	const user1, user2 = int64(71001), int64(71002)
	cleanBrowserNotificationTables(t, user1, user2)
	setValidVAPIDEnv(t)
	svc := NewBrowserNotificationService()

	firstInput := validBrowserSubscription("device-key-user-one-0001", "办公电脑", "https://push.example.com/sub-one")
	first, err := svc.UpsertSubscription(user1, firstInput)
	if err != nil {
		t.Fatalf("新增订阅失败: %v", err)
	}
	if !first.HasWebPush || first.Name != "办公电脑" {
		t.Fatalf("新增设备视图不符: %+v", first)
	}
	var stored model.WebPushSubscription
	if err := common.DB.Where("user_id = ? AND device_id = ?", user1, first.ID).First(&stored).Error; err != nil {
		t.Fatalf("读取订阅失败: %v", err)
	}
	if stored.EndpointCipher == firstInput.Endpoint || stored.P256dhCipher == firstInput.P256dh || stored.AuthCipher == firstInput.Auth {
		t.Fatal("Web Push 敏感字段必须加密落库")
	}
	if plain, _ := common.Decrypt(stored.EndpointCipher); plain != firstInput.Endpoint {
		t.Fatalf("endpoint 密文无法还原，得到 %q", plain)
	}

	updatedInput := validBrowserSubscription(firstInput.DeviceKey, "家用电脑", "https://push.example.com/sub-one-new")
	updated, err := svc.UpsertSubscription(user1, updatedInput)
	if err != nil || updated.ID != first.ID || updated.Name != "家用电脑" {
		t.Fatalf("更新订阅失败: view=%+v err=%v", updated, err)
	}
	common.DB.Where("user_id = ? AND device_id = ?", user1, first.ID).First(&stored)
	if plain, _ := common.Decrypt(stored.EndpointCipher); plain != updatedInput.Endpoint {
		t.Fatalf("更新后 endpoint 不符: %q", plain)
	}

	second, err := svc.UpsertSubscription(user1, validBrowserSubscription("device-key-user-one-0002", "手机", "https://push.example.com/sub-two"))
	if err != nil {
		t.Fatalf("新增第二设备失败: %v", err)
	}
	devices, err := svc.ListDevices(user1)
	if err != nil || len(devices) != 2 {
		t.Fatalf("多设备列表不符: len=%d err=%v", len(devices), err)
	}
	otherDevices, err := svc.ListDevices(user2)
	if err != nil || len(otherDevices) != 0 {
		t.Fatalf("跨用户读取必须为空: len=%d err=%v", len(otherDevices), err)
	}
	if err := svc.RemoveDevice(user2, first.ID); err == nil {
		t.Fatal("跨用户删除设备应拒绝")
	}
	if _, err := svc.UpsertSubscription(user2, validBrowserSubscription("device-key-user-two-0001", "越权设备", updatedInput.Endpoint)); err == nil {
		t.Fatal("跨用户绑定已有 endpoint 应拒绝")
	}
	if err := svc.RemoveDevice(user1, second.ID); err != nil {
		t.Fatalf("本人取消订阅失败: %v", err)
	}
	devices, _ = svc.ListDevices(user1)
	if len(devices) != 1 || devices[0].ID != first.ID {
		t.Fatalf("取消后设备列表不符: %+v", devices)
	}
}

func TestBrowserNotificationIdempotencyUpgradeAndFailureIsolation(t *testing.T) {
	const userID = int64(71001)
	cleanBrowserNotificationTables(t, userID)
	setValidVAPIDEnv(t)
	fake := &fakeBrowserPushSender{results: map[string]fakeBrowserPushResult{
		"https://push.example.com/expired": {status: http.StatusGone, err: errors.New("gone")},
	}}
	svc := &BrowserNotificationService{sender: fake, now: NewBrowserNotificationService().now}

	good, err := svc.UpsertSubscription(userID, validBrowserSubscription("device-key-notify-good-001", "电脑", "https://push.example.com/good"))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := svc.UpsertSubscription(userID, validBrowserSubscription("device-key-notify-bad-0002", "旧手机", "https://push.example.com/expired"))
	if err != nil {
		t.Fatal(err)
	}
	foreground, err := svc.UpsertSubscription(userID, BrowserSubscriptionInput{DeviceKey: "device-key-foreground-003", Name: "前台浏览器"})
	if err != nil {
		t.Fatal(err)
	}

	review := BrowserNotificationInput{SourceType: "position_exit_assessment", SourceID: 9981,
		FactKey:  BrowserFactKey("position_exit_assessment", "9981", "review", "fact-a"),
		Category: model.BrowserNotifyCategoryExitRisk, Level: "review", Title: "贵州茅台（600519）需要复核",
		Body: "主因：跌破风险阈值。下一步：打开持仓核对。", Route: "/positions?position_id=88&assessment_id=9981"}
	event, err := svc.CreateAndDispatch(context.Background(), userID, review, "")
	if err != nil || event == nil {
		t.Fatalf("声明 review 事件失败: event=%+v err=%v", event, err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("只有两台 Web Push 设备应发送，得到 %d", len(fake.calls))
	}
	var deliveries []model.BrowserNotificationDelivery
	common.DB.Where("user_id = ? AND event_id = ?", userID, event.ID).Order("device_id").Find(&deliveries)
	if len(deliveries) != 3 {
		t.Fatalf("三台设备应各有投递记录，得到 %d", len(deliveries))
	}
	var failed, delivered, pending int
	for _, delivery := range deliveries {
		switch delivery.Status {
		case model.BrowserDeliveryFailed:
			failed++
		case model.BrowserDeliveryDelivered:
			delivered++
		case model.BrowserDeliveryPending:
			pending++
		}
	}
	if failed != 1 || delivered != 1 || pending != 1 {
		t.Fatalf("单设备失败不能影响其他设备: failed=%d delivered=%d pending=%d", failed, delivered, pending)
	}
	var expiredSub model.WebPushSubscription
	common.DB.Where("user_id = ? AND device_id = ?", userID, expired.ID).First(&expiredSub)
	if expiredSub.Enabled || expiredSub.LastErrorCode != "subscription_expired" {
		t.Fatalf("410 应禁用失效订阅: %+v", expiredSub)
	}
	var expiredDevice model.BrowserNotificationDevice
	common.DB.First(&expiredDevice, expired.ID)
	if expiredDevice.HasWebPush {
		t.Fatal("410 后设备不得继续标记为 Web Push 已订阅")
	}

	if _, err := svc.CreateAndDispatch(context.Background(), userID, review, ""); err != nil {
		t.Fatalf("重复事实应幂等返回: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("重复事实不得再次发送，calls=%d", len(fake.calls))
	}
	var eventCount int64
	common.DB.Model(&model.BrowserNotificationEvent{}).Where("user_id = ?", userID).Count(&eventCount)
	if eventCount != 1 {
		t.Fatalf("重复事实只能有一个事件，得到 %d", eventCount)
	}

	urgent := review
	urgent.Level = "urgent"
	urgent.Title = "贵州茅台（600519）紧急处理"
	urgent.FactKey = BrowserFactKey("position_exit_assessment", "9982", "urgent", "fact-b")
	urgent.SourceID = 9982
	urgent.Route = "/positions?position_id=88&assessment_id=9982"
	if _, err := svc.CreateAndDispatch(context.Background(), userID, urgent, ""); err != nil {
		t.Fatalf("等级升级应生成新提醒: %v", err)
	}
	common.DB.Model(&model.BrowserNotificationEvent{}).Where("user_id = ?", userID).Count(&eventCount)
	if eventCount != 2 {
		t.Fatalf("review 升 urgent 应产生新事件，得到 %d", eventCount)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("失效设备已禁用，升级后仅有效 Web Push 设备再发一次，calls=%d", len(fake.calls))
	}

	rows, err := svc.PendingEvents(userID, "device-key-foreground-003", 0, 20)
	if err != nil || len(rows) != 2 {
		t.Fatalf("前台设备应轮询到两级事件: len=%d err=%v", len(rows), err)
	}
	if err := svc.Ack(userID+1, rows[0].DeliveryID, "device-key-foreground-003"); err == nil {
		t.Fatal("跨用户确认投递应拒绝")
	}
	if err := svc.Ack(userID, rows[0].DeliveryID, "device-key-notify-good-001"); err == nil {
		t.Fatal("跨设备确认投递应拒绝")
	}
	if err := svc.Ack(userID, rows[0].DeliveryID, "device-key-foreground-003"); err != nil {
		t.Fatalf("当前设备确认失败: %v", err)
	}
	_ = good
	_ = foreground
}

func TestBrowserNotificationVAPIDGateRouteAndDestinationModes(t *testing.T) {
	const userID = int64(71003)
	cleanBrowserNotificationTables(t, userID)
	t.Setenv("VAPID_PUBLIC_KEY", "")
	t.Setenv("VAPID_PRIVATE_KEY", "")
	t.Setenv("VAPID_SUBJECT", "")
	browser := NewBrowserNotificationService()
	config, err := browser.Config(userID)
	if err != nil || config.VAPIDConfigured || config.VAPIDPublicKey != "" {
		t.Fatalf("VAPID 缺失时必须正常启动且不返回公钥: %+v err=%v", config, err)
	}
	if !config.Settings.ExitRisk || !config.Settings.ManualAlert || config.Settings.Guard {
		t.Fatalf("浏览器分类默认值不符: %+v", config.Settings)
	}
	partial := validBrowserSubscription("device-key-no-vapid-0002", "不完整", "https://push.example.com/incomplete")
	partial.Auth = ""
	if _, err := browser.UpsertSubscription(userID, partial); err == nil {
		t.Fatal("不完整 Web Push 订阅应拒绝")
	}
	if _, err := browser.UpsertSubscription(userID, validBrowserSubscription("device-key-no-vapid-0001", "浏览器", "https://push.example.com/no-vapid")); err == nil {
		t.Fatal("VAPID 缺失时 Web Push 订阅应给出明确错误")
	}
	if _, err := browser.UpsertSubscription(userID, BrowserSubscriptionInput{DeviceKey: "device-key-no-vapid-0001", Name: "前台浏览器"}); err != nil {
		t.Fatalf("VAPID 缺失不能阻断前台 Notification: %v", err)
	}

	notify := &NotifyService{browser: browser}
	if !notify.HasEnabledDestination(userID, model.BrowserNotifyCategoryExitRisk) {
		t.Fatal("仅浏览器设备也应是有效通知目的地")
	}
	if enabled, err := notificationsEnabledFor(context.Background(), userID, model.BrowserNotifyCategoryExitRisk); err != nil || !enabled {
		t.Fatalf("仅浏览器目的地应通过通知总闸检查: enabled=%v err=%v", enabled, err)
	}
	if notify.HasEnabledDestination(userID, model.BrowserNotifyCategoryGuard) {
		t.Fatal("智能守护浏览器分类默认应关闭")
	}
	if _, err := browser.UpdateSettings(userID, BrowserNotificationSettingsInput{ExitRisk: true, ManualAlert: true, Guard: true}); err != nil {
		t.Fatal(err)
	}
	if !notify.HasEnabledDestination(userID, model.BrowserNotifyCategoryGuard) {
		t.Fatal("显式开启后智能守护应有浏览器目的地")
	}

	input := BrowserNotificationInput{SourceType: "alert_event", SourceID: 123,
		FactKey: BrowserFactKey("alert_event", "123"), Category: model.BrowserNotifyCategoryAlert,
		Level: "review", Title: "提醒", Body: "命中", Route: "//evil.example.com/steal"}
	event, err := browser.CreateAndDispatch(context.Background(), userID, input, "")
	if err != nil || event == nil || event.Route != "/" {
		t.Fatalf("开放重定向必须收口为站内根路由: event=%+v err=%v", event, err)
	}

	common.DB.Model(&model.UserPreference{}).Where("user_id = ?", userID).Update("enable_notify", false)
	blocked := input
	blocked.SourceID = 124
	blocked.FactKey = BrowserFactKey("alert_event", "124")
	if event, err := browser.CreateAndDispatch(context.Background(), userID, blocked, ""); err != nil || event != nil {
		t.Fatalf("总闸关闭应静默阻止浏览器事件: event=%+v err=%v", event, err)
	}
	common.DB.Model(&model.UserPreference{}).Where("user_id = ?", userID).Update("enable_notify", true)

	cipher, _ := common.Encrypt("SCT_FAKE_KEY")
	channel := model.NotifyChannel{UserID: userID, Kind: model.NotifyKindServerChan, Name: "外部", TargetCipher: cipher, Enabled: true}
	if err := common.DB.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	if !notify.HasEnabledDestination(userID, model.BrowserNotifyCategoryExitRisk) {
		t.Fatal("浏览器与外部通道同时开启时应有目的地")
	}
	if enabled, err := notificationsEnabledFor(context.Background(), userID, model.BrowserNotifyCategoryExitRisk); err != nil || !enabled {
		t.Fatalf("两类通道同时启用应通过检查: enabled=%v err=%v", enabled, err)
	}
	if err := browser.RemoveDevice(userID, configDeviceID(t, browser, userID)); err != nil {
		t.Fatal(err)
	}
	if !notify.HasEnabledDestination(userID, model.BrowserNotifyCategoryExitRisk) {
		t.Fatal("仅外部通道时仍应有目的地")
	}
	if enabled, err := notificationsEnabledFor(context.Background(), userID, model.BrowserNotifyCategoryExitRisk); err != nil || !enabled {
		t.Fatalf("仅外部通道应通过通知总闸检查: enabled=%v err=%v", enabled, err)
	}

	t.Setenv("VAPID_PUBLIC_KEY", "bad")
	t.Setenv("VAPID_PRIVATE_KEY", "bad")
	t.Setenv("VAPID_SUBJECT", "javascript:bad")
	if currentWebPushConfig().Configured {
		t.Fatal("无效 VAPID 配置不得标记为可用")
	}
}

func TestBrowserNotificationExpiredSubscriptionCleanup404And410(t *testing.T) {
	const userID = int64(71002)
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			cleanBrowserNotificationTables(t, userID)
			setValidVAPIDEnv(t)
			endpoint := fmt.Sprintf("https://push.example.com/expired-%d", status)
			fake := &fakeBrowserPushSender{results: map[string]fakeBrowserPushResult{
				endpoint: {status: status, err: fmt.Errorf("HTTP %d", status)},
			}}
			svc := &BrowserNotificationService{sender: fake, now: NewBrowserNotificationService().now}
			device, err := svc.UpsertSubscription(userID, validBrowserSubscription(
				fmt.Sprintf("device-key-expired-%d-0001", status), "失效设备", endpoint,
			))
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.CreateAndDispatch(context.Background(), userID, BrowserNotificationInput{
				SourceType: "alert_event", SourceID: int64(status), FactKey: BrowserFactKey("expired", fmt.Sprint(status)),
				Category: model.BrowserNotifyCategoryAlert, Level: "review", Title: "测试", Body: "测试", Route: "/alerts",
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			var sub model.WebPushSubscription
			common.DB.Where("user_id = ? AND device_id = ?", userID, device.ID).First(&sub)
			if sub.Enabled || sub.LastErrorCode != "subscription_expired" {
				t.Fatalf("HTTP %d 后订阅未自动禁用: %+v", status, sub)
			}
		})
	}
}

func configDeviceID(t *testing.T, svc *BrowserNotificationService, userID int64) int64 {
	t.Helper()
	devices, err := svc.ListDevices(userID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("读取当前设备失败: %+v err=%v", devices, err)
	}
	return devices[0].ID
}

type fakeNotifyChannelSender struct {
	calls []string
	err   error
}

type blockingNotifyChannelSender struct {
	started chan struct{}
	release chan struct{}
}

func (f *blockingNotifyChannelSender) Send(ctx context.Context, _, _ string, _ NotifyMessage) error {
	select {
	case f.started <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		return errors.New("fake delivery failed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeNotifyChannelSender) Send(_ context.Context, kind, target string, msg NotifyMessage) error {
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%s", kind, target, msg.Title))
	return f.err
}

func TestNotifyChannelTestSwitchAndSensitiveRetention(t *testing.T) {
	const user1, user2 = int64(71001), int64(71002)
	cleanBrowserNotificationTables(t, user1, user2)
	fake := &fakeNotifyChannelSender{}
	svc := &NotifyService{browser: NewBrowserNotificationService(), channelSender: fake}
	raw := `{"url":"https://ntfy.example.com","topic":"qv-u1","token":"tk_secret"}`
	view, err := svc.Create(user1, NotifyChannelInput{Kind: model.NotifyKindNtfy, Name: "手机", Target: raw, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	serverChan, err := svc.Create(user1, NotifyChannelInput{Kind: model.NotifyKindServerChan, Name: "Server酱", Target: "SCT_FAKE", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	webhook, err := svc.Create(user1, NotifyChannelInput{Kind: model.NotifyKindWebhook, Name: "Webhook", Target: "https://hook.example.com/fake", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{view.ID, serverChan.ID, webhook.ID} {
		if err := svc.Test(user1, id); err != nil {
			t.Fatalf("测试通道 %d 失败: %v", id, err)
		}
	}
	if len(fake.calls) != 3 || !strings.Contains(fake.calls[0], "tk_secret") ||
		!strings.Contains(strings.Join(fake.calls, "\n"), "SCT_FAKE") ||
		!strings.Contains(strings.Join(fake.calls, "\n"), "https://hook.example.com/fake") {
		t.Fatalf("测试发送必须使用保存配置且只走 fake transport: calls=%v err=%v", fake.calls, err)
	}
	if err := svc.Test(user2, view.ID); err == nil {
		t.Fatal("跨用户测试通道应拒绝")
	}
	if _, err := svc.Update(user1, view.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Name: "手机改名", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	var stored model.NotifyChannel
	common.DB.First(&stored, view.ID)
	if plain, _ := common.Decrypt(stored.TargetCipher); plain != raw || stored.Enabled {
		t.Fatalf("编辑全部敏感字段留空必须保留原配置并可停用: plain=%q enabled=%v", plain, stored.Enabled)
	}
	replacement := `{"url":"https://ntfy2.example.com","topic":"qv-u2","token":"tk_new"}`
	if _, err := svc.Update(user1, view.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Name: "新手机", Target: replacement, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	common.DB.First(&stored, view.ID)
	if plain, _ := common.Decrypt(stored.TargetCipher); plain != replacement || !stored.Enabled {
		t.Fatalf("完整重填后应替换配置并启用: plain=%q enabled=%v", plain, stored.Enabled)
	}
	if _, err := svc.Update(user1, webhook.ID, NotifyChannelInput{Kind: model.NotifyKindWebhook, Name: "Webhook 改名", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	stored = model.NotifyChannel{}
	common.DB.First(&stored, webhook.ID)
	if plain, _ := common.Decrypt(stored.TargetCipher); plain != "https://hook.example.com/fake" || stored.Enabled {
		t.Fatalf("Webhook 编辑留空必须保留地址并允许停用: plain=%q enabled=%v", plain, stored.Enabled)
	}
}

func TestNotifyDetachedDeliveryPersistsFactBeforeExternalFailure(t *testing.T) {
	const userID = int64(71001)
	cleanBrowserNotificationTables(t, userID)
	browser := NewBrowserNotificationService()
	if _, err := browser.UpsertSubscription(userID, BrowserSubscriptionInput{
		DeviceKey: "device-key-detached-0001", Name: "前台设备",
	}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := common.Encrypt("SCT_FAKE_DETACHED")
	if err := common.DB.Create(&model.NotifyChannel{UserID: userID, Kind: model.NotifyKindServerChan,
		Name: "慢通道", TargetCipher: cipher, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	blocking := &blockingNotifyChannelSender{started: make(chan struct{}, 1), release: make(chan struct{})}
	notify := &NotifyService{browser: browser, channelSender: blocking}
	input := BrowserNotificationInput{SourceType: "position_exit_assessment", SourceID: 551,
		FactKey:  BrowserFactKey("position_exit_assessment", "551", "review"),
		Category: model.BrowserNotifyCategoryExitRisk, Level: "review", Title: "卖出风险", Body: "需要复核",
		Route: "/positions?position_id=55&assessment_id=551"}
	notify.SendMsgDetached(context.Background(), userID, NotifyMessage{Title: "卖出风险", BrowserEvents: []BrowserNotificationInput{input}})

	var event model.BrowserNotificationEvent
	if err := common.DB.Where("user_id = ? AND fact_key = ?", userID, input.FactKey).First(&event).Error; err != nil {
		t.Fatalf("返回前必须先持久化浏览器事件: %v", err)
	}
	select {
	case <-blocking.started:
		close(blocking.release)
	case <-time.After(time.Second):
		t.Fatal("外部投递未在后台启动")
	}
	// 外部失败不能删除或回滚已声明的业务通知事实。
	time.Sleep(20 * time.Millisecond)
	if err := common.DB.First(&event, event.ID).Error; err != nil {
		t.Fatalf("外部失败后浏览器事件必须保留: %v", err)
	}
}
