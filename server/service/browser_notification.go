package service

import (
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	webpush "github.com/SherClockHolmes/webpush-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	maxBrowserDevices       = 12
	maxBrowserEventPageSize = 50
	webPushTTL              = 300
)

type webPushConfig struct {
	PublicKey  string
	PrivateKey string
	Subject    string
	Configured bool
}

func currentWebPushConfig() webPushConfig {
	cfg := webPushConfig{
		PublicKey:  strings.TrimSpace(os.Getenv("VAPID_PUBLIC_KEY")),
		PrivateKey: strings.TrimSpace(os.Getenv("VAPID_PRIVATE_KEY")),
		Subject:    strings.TrimSpace(os.Getenv("VAPID_SUBJECT")),
	}
	if cfg.PublicKey == "" || cfg.PrivateKey == "" || cfg.Subject == "" {
		return cfg
	}
	pub, pubErr := base64.RawURLEncoding.DecodeString(strings.TrimRight(cfg.PublicKey, "="))
	priv, privErr := base64.RawURLEncoding.DecodeString(strings.TrimRight(cfg.PrivateKey, "="))
	subject, subjectErr := url.Parse(cfg.Subject)
	publicX, publicY := elliptic.Unmarshal(elliptic.P256(), pub)
	privateValue := new(big.Int).SetBytes(priv)
	privateValid := privErr == nil && len(priv) == 32 && privateValue.Sign() > 0 && privateValue.Cmp(elliptic.P256().Params().N) < 0
	keyPairMatches := false
	if privateValid && publicX != nil && publicY != nil {
		expectedX, expectedY := elliptic.P256().ScalarBaseMult(priv)
		keyPairMatches = expectedX.Cmp(publicX) == 0 && expectedY.Cmp(publicY) == 0
	}
	validSubject := subjectErr == nil &&
		((subject.Scheme == "mailto" && strings.TrimSpace(subject.Opaque) != "") ||
			(subject.Scheme == "https" && subject.Host != "" && subject.User == nil))
	cfg.Configured = pubErr == nil && len(pub) == 65 && keyPairMatches && validSubject
	return cfg
}

type BrowserNotificationSettingsInput struct {
	ExitRisk    bool `json:"exit_risk"`
	ManualAlert bool `json:"manual_alert"`
	Guard       bool `json:"guard"`
}

type BrowserSubscriptionInput struct {
	DeviceKey string `json:"device_key"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	P256dh    string `json:"p256dh"`
	Auth      string `json:"auth"`
}

type BrowserDeviceView struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	HasWebPush    bool       `json:"has_web_push"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastFailureAt *time.Time `json:"last_failure_at"`
	LastErrorCode string     `json:"last_error_code"`
	CreatedAt     time.Time  `json:"created_at"`
}

type BrowserNotificationConfigView struct {
	VAPIDConfigured bool                             `json:"vapid_configured"`
	VAPIDPublicKey  string                           `json:"vapid_public_key"`
	Settings        BrowserNotificationSettingsInput `json:"settings"`
	Devices         []BrowserDeviceView              `json:"devices"`
}

type BrowserEventView struct {
	DeliveryID int64                          `json:"delivery_id"`
	Event      model.BrowserNotificationEvent `json:"event"`
}

type browserPushPlainSubscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

type browserPushSender interface {
	Send(context.Context, []byte, browserPushPlainSubscription) (int, error)
}

type webPushSender struct {
	config webPushConfig
	client webpush.HTTPClient
}

func (s webPushSender) Send(ctx context.Context, payload []byte, sub browserPushPlainSubscription) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient: s.client, Subscriber: s.config.Subject,
		VAPIDPublicKey: s.config.PublicKey, VAPIDPrivateKey: s.config.PrivateKey,
		TTL: webPushTTL,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("Web Push HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

type BrowserNotificationService struct {
	sender        browserPushSender
	now           func() time.Time
	asyncDelivery bool
}

func NewBrowserNotificationService() *BrowserNotificationService {
	cfg := currentWebPushConfig()
	var sender browserPushSender
	if cfg.Configured {
		sender = webPushSender{config: cfg, client: common.SafeHTTPClient(notifyTimeout, false)}
	}
	return &BrowserNotificationService{sender: sender, now: time.Now, asyncDelivery: true}
}

func browserDefaultSettings() BrowserNotificationSettingsInput {
	return BrowserNotificationSettingsInput{ExitRisk: true, ManualAlert: true, Guard: false}
}

func (s *BrowserNotificationService) settings(userID int64) (BrowserNotificationSettingsInput, error) {
	var row model.BrowserNotificationPreference
	err := common.DB.Where("user_id = ?", userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return browserDefaultSettings(), nil
	}
	if err != nil {
		return BrowserNotificationSettingsInput{}, err
	}
	return BrowserNotificationSettingsInput{ExitRisk: row.ExitRisk, ManualAlert: row.ManualAlert, Guard: row.Guard}, nil
}

func (s *BrowserNotificationService) Config(userID int64) (*BrowserNotificationConfigView, error) {
	settings, err := s.settings(userID)
	if err != nil {
		return nil, err
	}
	devices, err := s.ListDevices(userID)
	if err != nil {
		return nil, err
	}
	cfg := currentWebPushConfig()
	view := &BrowserNotificationConfigView{VAPIDConfigured: cfg.Configured, Settings: settings, Devices: devices}
	if cfg.Configured {
		view.VAPIDPublicKey = cfg.PublicKey
	}
	return view, nil
}

func (s *BrowserNotificationService) UpdateSettings(userID int64, in BrowserNotificationSettingsInput) (BrowserNotificationSettingsInput, error) {
	row := model.BrowserNotificationPreference{UserID: userID, ExitRisk: in.ExitRisk, ManualAlert: in.ManualAlert, Guard: in.Guard}
	err := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"exit_risk", "manual_alert", "guard", "updated_at"}),
	}).Create(&row).Error
	return in, err
}

func browserDeviceKeyHash(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 16 || len(raw) > 200 {
		return "", errors.New("设备标识无效，请重新开启浏览器通知")
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), nil
}

func validateWebPushInput(in BrowserSubscriptionInput) (bool, error) {
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	in.P256dh = strings.TrimSpace(in.P256dh)
	in.Auth = strings.TrimSpace(in.Auth)
	any := in.Endpoint != "" || in.P256dh != "" || in.Auth != ""
	if !any {
		return false, nil
	}
	if in.Endpoint == "" || in.P256dh == "" || in.Auth == "" {
		return false, errors.New("Web Push 订阅不完整，请重新订阅")
	}
	u, err := url.Parse(in.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(in.Endpoint) > 2000 {
		return false, errors.New("Web Push endpoint 无效")
	}
	p256dh, err1 := base64.RawURLEncoding.DecodeString(strings.TrimRight(in.P256dh, "="))
	auth, err2 := base64.RawURLEncoding.DecodeString(strings.TrimRight(in.Auth, "="))
	if err1 != nil || len(p256dh) != 65 || p256dh[0] != 4 || err2 != nil || len(auth) < 16 {
		return false, errors.New("Web Push 密钥无效，请重新订阅")
	}
	return true, nil
}

func (s *BrowserNotificationService) UpsertSubscription(userID int64, in BrowserSubscriptionInput) (*BrowserDeviceView, error) {
	deviceHash, err := browserDeviceKeyHash(in.DeviceKey)
	if err != nil {
		return nil, err
	}
	hasPush, err := validateWebPushInput(in)
	if err != nil {
		return nil, err
	}
	if hasPush && !currentWebPushConfig().Configured {
		return nil, errors.New("服务端尚未配置 VAPID，当前只能使用网站打开期间的浏览器通知")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "当前浏览器"
	}
	name = truncateRunes(name, 80)
	now := s.now()
	var device model.BrowserNotificationDevice
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		lookup := tx.Where("user_id = ? AND device_key_hash = ?", userID, deviceHash).First(&device).Error
		if errors.Is(lookup, gorm.ErrRecordNotFound) {
			var count int64
			if err := tx.Model(&model.BrowserNotificationDevice{}).Where("user_id = ? AND enabled = ?", userID, true).Count(&count).Error; err != nil {
				return err
			}
			if count >= maxBrowserDevices {
				return fmt.Errorf("浏览器通知设备已达上限（%d）", maxBrowserDevices)
			}
			device = model.BrowserNotificationDevice{UserID: userID, DeviceKeyHash: deviceHash, Name: name, Enabled: true, HasWebPush: hasPush, LastSeenAt: &now}
			if err := tx.Create(&device).Error; err != nil {
				return err
			}
		} else if lookup != nil {
			return lookup
		} else {
			if err := tx.Model(&device).Updates(map[string]any{"name": name, "enabled": true, "has_web_push": hasPush, "last_seen_at": &now}).Error; err != nil {
				return err
			}
			device.Name, device.Enabled, device.HasWebPush, device.LastSeenAt = name, true, hasPush, &now
		}
		if hasPush {
			endpointCipher, err := common.Encrypt(strings.TrimSpace(in.Endpoint))
			if err != nil {
				return errors.New("Web Push endpoint 加密失败")
			}
			p256dhCipher, err := common.Encrypt(strings.TrimSpace(in.P256dh))
			if err != nil {
				return errors.New("Web Push 密钥加密失败")
			}
			authCipher, err := common.Encrypt(strings.TrimSpace(in.Auth))
			if err != nil {
				return errors.New("Web Push 密钥加密失败")
			}
			endpointHash := BrowserFactKey(strings.TrimSpace(in.Endpoint))
			var owner model.WebPushSubscription
			if err := tx.Where("endpoint_hash = ? AND NOT (user_id = ? AND device_id = ?)", endpointHash, userID, device.ID).First(&owner).Error; err == nil {
				return errors.New("该 Web Push 订阅已绑定其他设备")
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			row := model.WebPushSubscription{UserID: userID, DeviceID: device.ID, EndpointHash: endpointHash,
				EndpointCipher: endpointCipher, P256dhCipher: p256dhCipher, AuthCipher: authCipher, Enabled: true}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "user_id"}, {Name: "device_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"endpoint_hash", "endpoint_cipher", "p256dh_cipher", "auth_cipher", "enabled", "last_error_code", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Where("user_id = ? AND device_id = ?", userID, device.ID).Delete(&model.WebPushSubscription{}).Error; err != nil {
				return err
			}
		}
		// 显式开启设备即表达接收意愿，与新增启用外部通道保持一致。老账号通常已有
		// 偏好行；首次直接开启通知时也要先创建默认行，不能让 UPDATE 0 行后总闸仍关闭。
		var pref model.UserPreference
		if err := tx.Where(model.UserPreference{UserID: userID}).
			Attrs(model.UserPreference{MinCandidateAmount: defaultMinCandidateAmount}).FirstOrCreate(&pref).Error; err != nil {
			return err
		}
		return tx.Model(&pref).Update("enable_notify", true).Error
	})
	if err != nil {
		return nil, err
	}
	views, err := s.ListDevices(userID)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].ID == device.ID {
			return &views[i], nil
		}
	}
	return nil, errors.New("浏览器通知设备保存失败")
}

func (s *BrowserNotificationService) ListDevices(userID int64) ([]BrowserDeviceView, error) {
	type row struct {
		model.BrowserNotificationDevice
		LastSuccessAt *time.Time
		LastFailureAt *time.Time
		LastErrorCode string
	}
	var rows []row
	err := common.DB.Table("browser_notification_devices AS d").
		Select("d.*, s.last_success_at, s.last_failure_at, s.last_error_code").
		Joins("LEFT JOIN web_push_subscriptions AS s ON s.device_id = d.id AND s.user_id = d.user_id AND s.enabled = ?", true).
		Where("d.user_id = ? AND d.enabled = ?", userID, true).Order("d.updated_at DESC, d.id DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]BrowserDeviceView, 0, len(rows))
	for _, row := range rows {
		out = append(out, BrowserDeviceView{ID: row.ID, Name: row.Name, Enabled: row.Enabled,
			HasWebPush: row.HasWebPush, LastSeenAt: row.LastSeenAt, LastSuccessAt: row.LastSuccessAt,
			LastFailureAt: row.LastFailureAt, LastErrorCode: row.LastErrorCode, CreatedAt: row.CreatedAt})
	}
	return out, nil
}

func (s *BrowserNotificationService) RemoveDevice(userID, deviceID int64) error {
	return common.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.BrowserNotificationDevice{}).Where("id = ? AND user_id = ? AND enabled = ?", deviceID, userID, true).
			Updates(map[string]any{"enabled": false, "has_web_push": false})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("浏览器通知设备不存在")
		}
		return tx.Where("user_id = ? AND device_id = ?", userID, deviceID).Delete(&model.WebPushSubscription{}).Error
	})
}

func (s *BrowserNotificationService) categoryEnabled(userID int64, category string) (bool, error) {
	settings, err := s.settings(userID)
	if err != nil {
		return false, err
	}
	switch category {
	case model.BrowserNotifyCategoryExitRisk:
		return settings.ExitRisk, nil
	case model.BrowserNotifyCategoryAlert:
		return settings.ManualAlert, nil
	case model.BrowserNotifyCategoryGuard:
		return settings.Guard, nil
	case model.BrowserNotifyCategorySystem:
		return true, nil
	default:
		return false, nil
	}
}

func (s *BrowserNotificationService) HasEnabledDestination(userID int64, category string) bool {
	enabled, err := s.categoryEnabled(userID, category)
	if err != nil || !enabled {
		return false
	}
	var count int64
	return common.DB.Model(&model.BrowserNotificationDevice{}).
		Where("user_id = ? AND enabled = ?", userID, true).Count(&count).Error == nil && count > 0
}

type BrowserNotificationInput struct {
	SourceType string
	SourceID   int64
	FactKey    string
	Category   string
	Level      string
	Title      string
	Body       string
	Route      string
}

func BrowserFactKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func sanitizeInternalRoute(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") || len(raw) > 500 {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	return u.RequestURI()
}

func (s *BrowserNotificationService) CreateAndDispatch(ctx context.Context, userID int64, in BrowserNotificationInput, onlyDeviceHash string) (*model.BrowserNotificationEvent, error) {
	if !userNotifyEnabled(userID) {
		return nil, nil
	}
	enabled, err := s.categoryEnabled(userID, in.Category)
	if err != nil || !enabled {
		return nil, err
	}
	in.SourceType = strings.TrimSpace(in.SourceType)
	if in.SourceType == "" || in.SourceID <= 0 || strings.TrimSpace(in.FactKey) == "" {
		return nil, errors.New("浏览器通知缺少稳定业务来源")
	}
	var devices []model.BrowserNotificationDevice
	q := common.DB.WithContext(ctx).Where("user_id = ? AND enabled = ?", userID, true)
	if onlyDeviceHash != "" {
		q = q.Where("device_key_hash = ?", onlyDeviceHash)
	}
	if err := q.Order("id ASC").Find(&devices).Error; err != nil || len(devices) == 0 {
		return nil, err
	}
	event := model.BrowserNotificationEvent{UserID: userID, SourceType: truncateRunes(in.SourceType, 32), SourceID: in.SourceID,
		FactKey: truncateRunes(in.FactKey, 128), Category: truncateRunes(in.Category, 24), Level: truncateRunes(in.Level, 16),
		Title: truncateRunes(in.Title, 160), Body: truncateRunes(in.Body, 768), Route: sanitizeInternalRoute(in.Route), CreatedAt: s.now()}
	created := false
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}, {Name: "fact_key"}}, DoNothing: true}).Create(&event)
		if res.Error != nil {
			return res.Error
		}
		created = res.RowsAffected == 1
		if !created {
			return tx.Where("user_id = ? AND fact_key = ?", userID, event.FactKey).First(&event).Error
		}
		for _, device := range devices {
			delivery := model.BrowserNotificationDelivery{UserID: userID, DeviceID: device.ID, EventID: event.ID, Status: model.BrowserDeliveryPending}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&delivery).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || !created {
		return &event, err
	}
	if s.asyncDelivery {
		go func() {
			pushCtx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			s.dispatchWebPush(pushCtx, event, devices)
		}()
	} else {
		s.dispatchWebPush(ctx, event, devices)
	}
	return &event, nil
}

func (s *BrowserNotificationService) dispatchWebPush(ctx context.Context, event model.BrowserNotificationEvent, devices []model.BrowserNotificationDevice) {
	if s.sender == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"event_id": event.ID, "title": event.Title, "body": event.Body,
		"route": event.Route, "level": event.Level, "category": event.Category})
	for _, device := range devices {
		var sub model.WebPushSubscription
		if err := common.DB.WithContext(ctx).Where("user_id = ? AND device_id = ? AND enabled = ?", event.UserID, device.ID, true).First(&sub).Error; err != nil {
			continue
		}
		plain := browserPushPlainSubscription{}
		var err error
		if plain.Endpoint, err = common.Decrypt(sub.EndpointCipher); err == nil {
			plain.P256dh, err = common.Decrypt(sub.P256dhCipher)
		}
		if err == nil {
			plain.Auth, err = common.Decrypt(sub.AuthCipher)
		}
		status := 0
		if err == nil {
			status, err = s.sender.Send(ctx, payload, plain)
		}
		now := s.now()
		deliveryUpdates := map[string]any{"push_attempted_at": &now, "attempt_count": gorm.Expr("attempt_count + 1")}
		subUpdates := map[string]any{}
		if err == nil {
			deliveryUpdates["status"] = model.BrowserDeliveryDelivered
			deliveryUpdates["push_delivered_at"] = &now
			deliveryUpdates["last_error_code"] = ""
			subUpdates["last_success_at"] = &now
			subUpdates["last_error_code"] = ""
		} else {
			code := "push_failed"
			if status == http.StatusNotFound || status == http.StatusGone {
				code = "subscription_expired"
				subUpdates["enabled"] = false
				common.DB.Model(&model.BrowserNotificationDevice{}).Where("id = ? AND user_id = ?", device.ID, event.UserID).Update("has_web_push", false)
			}
			deliveryUpdates["status"] = model.BrowserDeliveryFailed
			deliveryUpdates["last_error_code"] = code
			subUpdates["last_failure_at"] = &now
			subUpdates["last_error_code"] = code
		}
		common.DB.Model(&model.BrowserNotificationDelivery{}).
			Where("user_id = ? AND device_id = ? AND event_id = ?", event.UserID, device.ID, event.ID).Updates(deliveryUpdates)
		common.DB.Model(&model.WebPushSubscription{}).Where("id = ? AND user_id = ?", sub.ID, event.UserID).Updates(subUpdates)
	}
}

func (s *BrowserNotificationService) PendingEvents(userID int64, deviceKey string, afterID int64, limit int) ([]BrowserEventView, error) {
	deviceHash, err := browserDeviceKeyHash(deviceKey)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxBrowserEventPageSize {
		limit = 20
	}
	var device model.BrowserNotificationDevice
	if err := common.DB.Where("user_id = ? AND device_key_hash = ? AND enabled = ?", userID, deviceHash, true).First(&device).Error; err != nil {
		return nil, errors.New("浏览器通知设备不存在")
	}
	now := s.now()
	common.DB.Model(&device).Update("last_seen_at", &now)
	type row struct {
		DeliveryID int64 `gorm:"column:delivery_id"`
		model.BrowserNotificationEvent
	}
	var rows []row
	err = common.DB.Table("browser_notification_deliveries AS d").
		Select("d.id AS delivery_id, e.*").
		Joins("JOIN browser_notification_events AS e ON e.id = d.event_id AND e.user_id = d.user_id").
		Where("d.user_id = ? AND d.device_id = ? AND d.foreground_ack_at IS NULL AND d.status IN ? AND e.id > ?",
			userID, device.ID, []string{model.BrowserDeliveryPending, model.BrowserDeliveryFailed}, afterID).
		Order("e.id ASC").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]BrowserEventView, 0, len(rows))
	for _, row := range rows {
		out = append(out, BrowserEventView{DeliveryID: row.DeliveryID, Event: row.BrowserNotificationEvent})
	}
	return out, nil
}

func (s *BrowserNotificationService) Ack(userID, deliveryID int64, deviceKey string) error {
	deviceHash, err := browserDeviceKeyHash(deviceKey)
	if err != nil {
		return err
	}
	var device model.BrowserNotificationDevice
	if err := common.DB.Where("user_id = ? AND device_key_hash = ? AND enabled = ?", userID, deviceHash, true).First(&device).Error; err != nil {
		return errors.New("浏览器通知设备不存在")
	}
	now := s.now()
	res := common.DB.Model(&model.BrowserNotificationDelivery{}).
		Where("id = ? AND user_id = ? AND device_id = ?", deliveryID, userID, device.ID).
		Updates(map[string]any{"foreground_ack_at": &now, "status": model.BrowserDeliveryDelivered})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("浏览器通知投递不存在")
	}
	return nil
}

func (s *BrowserNotificationService) Test(userID int64, deviceKey string) error {
	deviceHash, err := browserDeviceKeyHash(deviceKey)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.CreateAndDispatch(context.Background(), userID, BrowserNotificationInput{
		SourceType: "browser_test", SourceID: now.UnixNano(), FactKey: BrowserFactKey("browser_test", fmt.Sprint(userID), fmt.Sprint(now.UnixNano())),
		Category: model.BrowserNotifyCategorySystem, Level: "info", Title: "QuantVista 浏览器通知测试",
		Body: "浏览器通知已连接。点击可返回通知设置。", Route: "/settings?tab=notifications",
	}, deviceHash)
	return err
}
