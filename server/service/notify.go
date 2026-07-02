package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// NotifyService 主动推送：管理用户的推送通道（Server酱/自定义 webhook），提醒命中时推送。
// target（sendkey/url）加密落库；推送走 SafeHTTPClient 防 SSRF（禁内网，用户配置的外部通道）。
type NotifyService struct{}

func NewNotifyService() *NotifyService { return &NotifyService{} }

const (
	notifyTimeout      = 10 * time.Second
	maxChannelsPerUser = 10
)

var validNotifyKind = map[string]bool{
	model.NotifyKindServerChan: true,
	model.NotifyKindWebhook:    true,
}

// NotifyChannelView 通道视图（不含密文，附 has_target）。
type NotifyChannelView struct {
	model.NotifyChannel
	HasTarget bool `json:"has_target"`
}

func toChannelView(ch model.NotifyChannel) NotifyChannelView {
	has := ch.TargetCipher != ""
	ch.TargetCipher = ""
	return NotifyChannelView{NotifyChannel: ch, HasTarget: has}
}

// NotifyChannelInput 增改入参。Target 为明文；更新时留空表示保留原值。
type NotifyChannelInput struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
}

func (s *NotifyService) validate(in *NotifyChannelInput, requireTarget bool) error {
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if !validNotifyKind[in.Kind] {
		return errors.New("不支持的推送类型")
	}
	in.Target = strings.TrimSpace(in.Target)
	if requireTarget && in.Target == "" {
		return errors.New("请填写 sendkey 或 webhook 地址")
	}
	if in.Kind == model.NotifyKindWebhook && in.Target != "" {
		u, err := url.Parse(in.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("webhook 地址非法（仅支持 http/https）")
		}
	}
	return nil
}

// userNotifyEnabled 用户偏好「开启提醒」是否打开（推送总闸，见 Settings 页开关）。
func userNotifyEnabled(userID int64) bool {
	var pref model.UserPreference
	if err := common.DB.Where("user_id = ?", userID).First(&pref).Error; err != nil {
		return false
	}
	return pref.EnableNotify
}

// List 列出用户的推送通道（不含密文）。
func (s *NotifyService) List(userID int64) ([]NotifyChannelView, error) {
	var rows []model.NotifyChannel
	if err := common.DB.Where("user_id = ?", userID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]NotifyChannelView, 0, len(rows))
	for _, r := range rows {
		out = append(out, toChannelView(r))
	}
	return out, nil
}

// Create 新建通道（target 加密）。
func (s *NotifyService) Create(userID int64, in NotifyChannelInput) (*NotifyChannelView, error) {
	if err := s.validate(&in, true); err != nil {
		return nil, err
	}
	var cnt int64
	common.DB.Model(&model.NotifyChannel{}).Where("user_id = ?", userID).Count(&cnt)
	if cnt >= maxChannelsPerUser {
		return nil, fmt.Errorf("推送通道数量已达上限（%d）", maxChannelsPerUser)
	}
	cipher, err := common.Encrypt(in.Target)
	if err != nil {
		return nil, errors.New("加密失败")
	}
	ch := model.NotifyChannel{
		UserID: userID, Kind: in.Kind, Name: strings.TrimSpace(in.Name),
		TargetCipher: cipher, Enabled: in.Enabled,
	}
	if ch.Name == "" {
		ch.Name = defaultChannelName(in.Kind)
	}
	if err := common.DB.Create(&ch).Error; err != nil {
		return nil, err
	}
	// 新配置启用通道即视为明确的推送意愿，顺手打开偏好总闸
	// （否则 enable_notify 默认 false，配好通道也收不到推送，易误判为故障）。
	if ch.Enabled {
		common.DB.Model(&model.UserPreference{}).Where("user_id = ?", userID).
			Update("enable_notify", true)
	}
	v := toChannelView(ch)
	return &v, nil
}

// Update 更新通道。Target 留空保留原密文。
func (s *NotifyService) Update(userID, id int64, in NotifyChannelInput) (*NotifyChannelView, error) {
	if err := s.validate(&in, false); err != nil {
		return nil, err
	}
	var ch model.NotifyChannel
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&ch).Error; err != nil {
		return nil, errors.New("推送通道不存在")
	}
	ch.Kind = in.Kind
	ch.Name = strings.TrimSpace(in.Name)
	if ch.Name == "" {
		ch.Name = defaultChannelName(in.Kind)
	}
	ch.Enabled = in.Enabled
	if in.Target != "" {
		cipher, err := common.Encrypt(in.Target)
		if err != nil {
			return nil, errors.New("加密失败")
		}
		ch.TargetCipher = cipher
	}
	if err := common.DB.Save(&ch).Error; err != nil {
		return nil, err
	}
	v := toChannelView(ch)
	return &v, nil
}

// Delete 删除通道。
func (s *NotifyService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.NotifyChannel{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("推送通道不存在")
	}
	return nil
}

// Test 向指定通道发送一条测试消息。
func (s *NotifyService) Test(userID, id int64) error {
	var ch model.NotifyChannel
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&ch).Error; err != nil {
		return errors.New("推送通道不存在")
	}
	return s.sendTo(ch, "QuantVista 测试推送", "这是一条来自 QuantVista 的测试消息，收到即表示通道配置正确。")
}

// Send 向用户所有启用的通道推送一条消息（best-effort，逐个通道独立成败）。
func (s *NotifyService) Send(userID int64, title, content string) {
	var rows []model.NotifyChannel
	if err := common.DB.Where("user_id = ? AND enabled = ?", userID, true).Find(&rows).Error; err != nil {
		return
	}
	for _, ch := range rows {
		_ = s.sendTo(ch, title, content)
	}
}

// HasEnabledChannel 用户是否配置了启用的通道（供提醒评估判断是否需要推送）。
func (s *NotifyService) HasEnabledChannel(userID int64) bool {
	var cnt int64
	common.DB.Model(&model.NotifyChannel{}).Where("user_id = ? AND enabled = ?", userID, true).Count(&cnt)
	return cnt > 0
}

// sendTo 向单个通道发送，并回写 last_sent_at/last_error。
func (s *NotifyService) sendTo(ch model.NotifyChannel, title, content string) error {
	target, err := common.Decrypt(ch.TargetCipher)
	if err != nil || target == "" {
		if err == nil {
			err = errors.New("通道密钥缺失或解密失败")
		}
		s.recordResult(ch.ID, err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	switch ch.Kind {
	case model.NotifyKindServerChan:
		err = sendServerChan(ctx, target, title, content)
	case model.NotifyKindWebhook:
		err = sendWebhook(ctx, target, title, content)
	default:
		err = errors.New("未知通道类型")
	}
	s.recordResult(ch.ID, err)
	return err
}

func (s *NotifyService) recordResult(id int64, err error) {
	now := time.Now()
	upd := map[string]any{"last_sent_at": &now}
	if err != nil {
		upd["last_error"] = truncateRunes(err.Error(), 256)
	} else {
		upd["last_error"] = ""
	}
	common.DB.Model(&model.NotifyChannel{}).Where("id = ?", id).Updates(upd)
}

// sendServerChan 走 Server酱 sctapi.ftqq.com/{sendkey}.send（表单 title+desp）。
func sendServerChan(ctx context.Context, sendkey, title, content string) error {
	endpoint := "https://sctapi.ftqq.com/" + url.PathEscape(sendkey) + ".send"
	form := url.Values{"title": {title}, "desp": {content}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := common.SafeHTTPClient(notifyTimeout, false) // 禁内网，防 SSRF
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Server酱 HTTP %d: %s", resp.StatusCode, extractErr(raw))
	}
	return nil
}

// sendWebhook POST JSON {title,content} 到用户配置的地址。
func sendWebhook(ctx context.Context, target, title, content string) error {
	body, _ := json.Marshal(map[string]string{"title": title, "content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := common.SafeHTTPClient(notifyTimeout, false)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d: %s", resp.StatusCode, extractErr(raw))
	}
	return nil
}

func defaultChannelName(kind string) string {
	if kind == model.NotifyKindServerChan {
		return "Server酱"
	}
	return "Webhook"
}
