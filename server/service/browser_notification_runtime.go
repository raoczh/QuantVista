package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 事件先持久化，再唤醒本进程长轮询。跨进程由有界复查补偿，不要求 Redis。
var browserWaiters = struct {
	sync.Mutex
	users map[int64]map[chan struct{}]struct{}
}{users: make(map[int64]map[chan struct{}]struct{})}

func watchBrowserNotifications(userID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	browserWaiters.Lock()
	if browserWaiters.users[userID] == nil {
		browserWaiters.users[userID] = make(map[chan struct{}]struct{})
	}
	browserWaiters.users[userID][ch] = struct{}{}
	browserWaiters.Unlock()
	return ch, func() {
		browserWaiters.Lock()
		defer browserWaiters.Unlock()
		delete(browserWaiters.users[userID], ch)
		if len(browserWaiters.users[userID]) == 0 {
			delete(browserWaiters.users, userID)
		}
	}
}

func wakeBrowserNotifications(userID int64) {
	browserWaiters.Lock()
	defer browserWaiters.Unlock()
	for ch := range browserWaiters.users[userID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *BrowserNotificationService) WaitPendingEvents(ctx context.Context, userID int64, deviceKey string, afterID int64, limit, seconds int) ([]BrowserEventView, error) {
	if seconds <= 0 {
		return s.pendingEventsContext(ctx, userID, deviceKey, afterID, limit)
	}
	if seconds > 25 {
		seconds = 25
	}
	wake, unwatch := watchBrowserNotifications(userID)
	defer unwatch()
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()
	recheck := time.NewTicker(5 * time.Second)
	defer recheck.Stop()
	for {
		rows, err := s.pendingEventsContext(ctx, userID, deviceKey, afterID, limit)
		if err != nil || len(rows) > 0 {
			return rows, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return rows, nil
		case <-wake:
		case <-recheck.C:
		}
	}
}

func browserPushRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	return time.Duration(15<<(attempt-1)) * time.Second
}

func (s *BrowserNotificationService) claimPushDelivery(ctx context.Context, event model.BrowserNotificationEvent, device model.BrowserNotificationDevice) (*model.BrowserNotificationDelivery, string, error) {
	now := s.now()
	if event.CreatedAt.Before(now.Add(-time.Duration(webPushTTL) * time.Second)) {
		return nil, "", nil
	}
	lease := uuid.NewString()
	var row model.BrowserNotificationDelivery
	err := common.DB.WithContext(ctx).Where("user_id = ? AND device_id = ? AND event_id = ?", event.UserID, device.ID, event.ID).First(&row).Error
	if err != nil {
		return nil, "", err
	}
	res := common.DB.WithContext(ctx).Model(&model.BrowserNotificationDelivery{}).
		Where("id = ? AND foreground_ack_at IS NULL AND push_delivered_at IS NULL AND status IN ? AND next_push_at_ms <= ? AND push_lease_until_ms <= ?",
			row.ID, []string{model.BrowserDeliveryPending, model.BrowserDeliveryFailed}, now.UnixMilli(), now.UnixMilli()).
		Where("EXISTS (SELECT 1 FROM browser_notification_devices d WHERE d.id = ? AND d.user_id = ? AND d.enabled = ?)", device.ID, event.UserID, true).
		Updates(map[string]any{"push_lease_token": lease, "push_lease_until_ms": now.Add(notifyTimeout + 10*time.Second).UnixMilli(),
			"push_attempted_at": &now, "attempt_count": gorm.Expr("attempt_count + 1")})
	if res.Error != nil || res.RowsAffected != 1 {
		return nil, "", res.Error
	}
	row.AttemptCount++
	return &row, lease, nil
}

// RetryPendingPush 承接进程重启、请求超时和推送服务临时失败。超过五分钟不补弹旧行情；
// 业务事件/待办继续保留，HTTP 2xx 仅表示推送服务已接收，不宣称设备实际展示。
func (s *BrowserNotificationService) RetryPendingPush(ctx context.Context) error {
	if s.sender == nil || common.DB == nil {
		return nil
	}
	now := s.now()
	var rows []model.BrowserNotificationDelivery
	err := common.DB.WithContext(ctx).Table("browser_notification_deliveries AS d").Select("d.*").
		Joins("JOIN browser_notification_events e ON e.id = d.event_id AND e.user_id = d.user_id").
		Joins("JOIN browser_notification_devices v ON v.id = d.device_id AND v.user_id = d.user_id AND v.enabled = ?", true).
		Joins("JOIN web_push_subscriptions s ON s.device_id = d.device_id AND s.user_id = d.user_id AND s.enabled = ?", true).
		Where("d.foreground_ack_at IS NULL AND d.push_delivered_at IS NULL AND d.status IN ? AND d.next_push_at_ms <= ? AND d.push_lease_until_ms <= ? AND e.created_at >= ?",
			[]string{model.BrowserDeliveryPending, model.BrowserDeliveryFailed}, now.UnixMilli(), now.UnixMilli(), now.Add(-time.Duration(webPushTTL)*time.Second)).
		Order("d.next_push_at_ms, d.id").Limit(100).Find(&rows).Error
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var event model.BrowserNotificationEvent
		var device model.BrowserNotificationDevice
		if err := common.DB.WithContext(ctx).Where("id = ? AND user_id = ?", row.EventID, row.UserID).First(&event).Error; err != nil {
			return err
		}
		if err := common.DB.WithContext(ctx).Where("id = ? AND user_id = ? AND enabled = ?", row.DeviceID, row.UserID, true).First(&device).Error; err != nil {
			continue
		}
		s.dispatchWebPush(ctx, event, []model.BrowserNotificationDevice{device})
	}
	return nil
}

var browserPushJobsOnce sync.Once

func StartBrowserNotificationJobs() {
	browserPushJobsOnce.Do(func() {
		go func() {
			s := NewBrowserNotificationService()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				err := s.RetryPendingPush(ctx)
				cancel()
				if err != nil {
					common.SysWarn("浏览器后台通知重试暂不可用：%s", fmt.Sprintf("%T", err))
				}
				<-ticker.C
			}
		}()
	})
}
