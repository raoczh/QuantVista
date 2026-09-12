package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type reviewBrowserSender func(context.Context, []byte, browserPushPlainSubscription) (int, error)

func (send reviewBrowserSender) Send(ctx context.Context, body []byte, sub browserPushPlainSubscription) (int, error) {
	return send(ctx, body, sub)
}

type reviewNotifySender func(context.Context, string, string, NotifyMessage) error

func (send reviewNotifySender) Send(ctx context.Context, kind, target string, msg NotifyMessage) error {
	return send(ctx, kind, target, msg)
}

func seedNotificationReviewUser(t *testing.T, userID int64) {
	t.Helper()
	if err := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.User{ID: userID, Username: fmt.Sprintf("notification-review-%d", userID), Status: model.StatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	common.EncryptionKey = "notification-review-encryption-key"
}

func TestBrowserSettingsCanDisableDefaultCategories(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1122
	seedNotificationReviewUser(t, userID)
	svc := NewBrowserNotificationService()
	if _, err := svc.UpsertSubscription(userID, BrowserSubscriptionInput{DeviceKey: "review-category-device-key", Name: "前台设备"}); err != nil {
		t.Fatal(err)
	}
	for _, desired := range []BrowserNotificationSettingsInput{{}, {ExitRisk: true, ManualAlert: true}, {}} {
		if _, err := svc.UpdateSettings(userID, desired); err != nil {
			t.Fatal(err)
		}
		actual, err := svc.Config(userID)
		if err != nil || actual.Settings != desired {
			t.Fatalf("关闭分类必须持久化，不能被数据库默认 true 改回：desired=%+v actual=%+v err=%v", desired, actual, err)
		}
	}
	event, err := svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{
		SourceType: "alert_event", SourceID: 1, FactKey: "disabled-category-review", Category: model.BrowserNotifyCategoryAlert,
		Title: "不应投递", Body: "分类已关闭", Route: "/alerts",
	}, "")
	if err != nil || event != nil {
		t.Fatalf("关闭的分类不能创建投递事件：%+v %v", event, err)
	}
}

func TestBrowserRenewalIgnoresOldExpiration(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1123
	seedNotificationReviewUser(t, userID)
	setValidVAPIDEnv(t)
	svc := NewBrowserNotificationService()
	svc.asyncDelivery = false
	in := validBrowserSubscription("review-browser-renewal-key", "浏览器", "https://push.example.com/review-old")
	device, err := svc.UpsertSubscription(userID, in)
	if err != nil {
		t.Fatal(err)
	}
	svc.sender = reviewBrowserSender(func(context.Context, []byte, browserPushPlainSubscription) (int, error) {
		in.Endpoint = "https://push.example.com/review-renewed"
		if _, err := svc.UpsertSubscription(userID, in); err != nil {
			t.Error(err)
		}
		return http.StatusGone, errors.New("旧订阅已过期")
	})
	_, err = svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{
		SourceType: "alert_event", SourceID: 1, FactKey: "renewal-review", Category: model.BrowserNotifyCategoryAlert,
		Title: "测试事件", Body: "只通过本地假发送器", Route: "/alerts",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	var stored model.WebPushSubscription
	if err := common.DB.Where("user_id = ? AND device_id = ?", userID, device.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var current model.BrowserNotificationDevice
	if err := common.DB.First(&current, device.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled || stored.LastErrorCode != "" || !current.HasWebPush {
		t.Fatalf("旧请求的 410 不能禁用已续订的新地址：enabled=%v error=%s has_push=%v", stored.Enabled, stored.LastErrorCode, current.HasWebPush)
	}
}

func TestBrowserDeviceReactivationHonorsLimit(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1124
	seedNotificationReviewUser(t, userID)
	svc := NewBrowserNotificationService()
	old := BrowserSubscriptionInput{DeviceKey: "review-old-browser-device", Name: "已停用"}
	device, err := svc.UpsertSubscription(userID, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDevice(userID, device.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxBrowserDevices; i++ {
		if _, err := svc.UpsertSubscription(userID, BrowserSubscriptionInput{DeviceKey: fmt.Sprintf("review-active-browser-%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.UpsertSubscription(userID, old); err == nil {
		t.Fatal("重新启用旧设备不能绕过有效设备上限")
	}
}

func TestNotifyEditDoesNotResurrectDeletedChannel(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1125
	seedNotificationReviewUser(t, userID)
	svc := NewNotifyService()
	channel, err := svc.Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_LOCAL_TEST"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	const callback = "review:notify_delete_after_edit_read"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(db *gorm.DB) {
		if row, ok := db.Statement.Dest.(*model.NotifyChannel); ok && row.ID == channel.ID && !called {
			called = true
			if err := svc.Delete(userID, row.ID); err != nil {
				t.Error(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	_, updateErr := svc.Update(userID, channel.ID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Name: "旧编辑"})
	var count int64
	if err := common.DB.Model(&model.NotifyChannel{}).Where("id = ?", channel.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !called || updateErr == nil || count != 0 {
		t.Fatalf("编辑不能复活删除通道：read=%v error=%v count=%d", called, updateErr, count)
	}
}

func TestNotifyCreationCreatesMissingPreference(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1126
	seedNotificationReviewUser(t, userID)
	if _, err := NewNotifyService().Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_LOCAL_TEST", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !userNotifyEnabled(userID) {
		t.Fatal("首次配置启用通道应同时持久化通知总开关")
	}
}

func TestNotifyPreferenceFailureRollsBackChannel(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1127
	seedNotificationReviewUser(t, userID)
	if err := common.DB.Create(&model.UserPreference{UserID: userID}).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review:notify_preference_failure"
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table == "user_preferences" {
			db.AddError(errors.New("注入通知偏好写入失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
	_, err := NewNotifyService().Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_LOCAL_TEST", Enabled: true})
	var count int64
	if readErr := common.DB.Model(&model.NotifyChannel{}).Where("user_id = ?", userID).Count(&count).Error; readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || count != 0 {
		t.Fatalf("偏好写入失败不能留下半份配置：error=%v count=%d", err, count)
	}
}

func TestNotifyLateResultStaysWithTestedConfiguration(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1128
	seedNotificationReviewUser(t, userID)
	svc := NewNotifyService()
	channel, err := svc.Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_OLD_LOCAL_TEST"})
	if err != nil {
		t.Fatal(err)
	}
	svc.channelSender = reviewNotifySender(func(context.Context, string, string, NotifyMessage) error {
		if _, err := svc.Update(userID, channel.ID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_NEW_LOCAL_TEST", Name: "新配置"}); err != nil {
			t.Error(err)
		}
		return errors.New("旧地址发送失败")
	})
	if err := svc.Test(userID, channel.ID); err == nil {
		t.Fatal("旧地址测试应失败")
	}
	rows, err := svc.List(userID)
	if err != nil || len(rows) != 1 || rows[0].LastError != "" || rows[0].LastSentAt != nil {
		t.Fatalf("旧地址的测试结果不能挂到新配置：rows=%+v err=%v", rows, err)
	}
}

func TestNotifyExternalRespectsMasterSwitch(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1129
	seedNotificationReviewUser(t, userID)
	fake := &fakeNotifyChannelSender{}
	svc := &NotifyService{channelSender: fake}
	if _, err := svc.Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT_LOCAL_TEST", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 兼容修复前没有自动建偏好行的情况，明确写入关闭的总闸。
	pref := model.UserPreference{UserID: userID}
	if err := common.DB.Where("user_id = ?", userID).FirstOrCreate(&pref).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&pref).Update("enable_notify", false).Error; err != nil {
		t.Fatal(err)
	}
	svc.SendMsgContext(t.Context(), userID, NotifyMessage{Title: "已关闭总闸"})
	if len(fake.calls) != 0 {
		t.Fatal("总开关关闭后外部通道也不能继续发送")
	}
}

func TestNotifyStatusUpdateKeepsNameAndTarget(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1134
	seedNotificationReviewUser(t, userID)
	svc := NewNotifyService()
	channel, err := svc.Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Name: "当前名称", Target: "SCT_LOCAL_TEST", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	state := false
	updated, err := svc.UpdateFields(userID, channel.ID, NotifyChannelUpdateInput{Enabled: &state})
	if err != nil || updated.Name != channel.Name || !updated.HasTarget || updated.Enabled {
		t.Fatalf("只切换状态不能改写名称或密钥：%+v %v", updated, err)
	}
}

func TestNotifyServerChanRequiresBusinessSuccess(t *testing.T) {
	for _, sample := range []struct {
		status int
		body   string
		ok     bool
	}{{200, `{"code":0,"data":{"pushid":"local"}}`, true}, {200, `{"code":40001,"message":"无效 sendkey"}`, false},
		{200, `{"message":"没有成功标记"}`, false}, {200, `<html>upstream unavailable</html>`, false}, {500, `{"code":0}`, false}} {
		if err := serverChanResponseError(sample.status, []byte(sample.body)); (err == nil) != sample.ok {
			t.Errorf("HTTP 状态不能替代业务成功：status=%d body=%s err=%v", sample.status, sample.body, err)
		}
	}
}

func TestNotifyErrorHidesTargetCredential(t *testing.T) {
	secret := "SCT_SECRET_LOCAL_ONLY"
	err := sanitizeNotifyError(&url.Error{Op: "Post", URL: "https://sctapi.ftqq.com/" + secret + ".send", Err: errors.New("network failed")}, model.NotifyKindServerChan, secret)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "ftqq.com") {
		t.Fatal("URL 错误不能回显通道凭证")
	}
	err = sanitizeNotifyError(errors.New("upstream rejected token tk_local_secret"), model.NotifyKindNtfy, `{"url":"https://ntfy.example.com","topic":"qv","token":"tk_local_secret"}`)
	if err == nil || strings.Contains(err.Error(), "tk_local_secret") {
		t.Fatal("上游错误中的 ntfy 令牌必须隐藏")
	}
}

func TestBrowserPendingEventsHonorDisabledPreferences(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1132
	seedNotificationReviewUser(t, userID)
	svc := NewBrowserNotificationService()
	in := BrowserSubscriptionInput{DeviceKey: "review-pending-category-key", Name: "前台"}
	if _, err := svc.UpsertSubscription(userID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAndDispatch(t.Context(), userID, BrowserNotificationInput{
		SourceType: "alert_event", SourceID: 1, FactKey: "pending-category-review", Category: model.BrowserNotifyCategoryAlert, Title: "尚未投递",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if rows, err := svc.PendingEvents(userID, in.DeviceKey, 0, 20); err != nil || len(rows) != 1 {
		t.Fatalf("原始事件应已排队：%+v %v", rows, err)
	}
	if _, err := svc.UpdateSettings(userID, BrowserNotificationSettingsInput{}); err != nil {
		t.Fatal(err)
	}
	if rows, err := svc.PendingEvents(userID, in.DeviceKey, 0, 20); err != nil || len(rows) != 0 {
		t.Fatalf("已关闭分类的旧排队事件不能继续弹出：%+v %v", rows, err)
	}
}
