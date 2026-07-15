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
	"quantvista/setting"
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
	model.NotifyKindNtfy:       true,
}

// NotifyMessage 推送消息（SendMsg 主入口的载荷）。
// Route/Kind/Priority 仅 ntfy 通道消费：Server酱/Webhook 保持 title+content 老格式零回归。
type NotifyMessage struct {
	Title   string
	Content string
	Route   string // 站内路由（/alerts、/daily-reports、/stock/600519）；ntfy 拼 click=<SiteBaseURL>+Route，SiteBaseURL 未配置则不带
	Kind    string // 消息类别（映射 ntfy tags 图标）：alert / earn / report / guard
	Priority int   // ntfy 优先级 1~5；0=默认（不下发字段）。止损触达等紧急事件给 4
}

// 消息类别（NotifyMessage.Kind）。
const (
	NotifyMsgKindAlert  = "alert"  // 条件提醒命中
	NotifyMsgKindEarn   = "earn"   // 财报提醒
	NotifyMsgKindReport = "report" // 收盘日报
	NotifyMsgKindGuard  = "guard"  // 守护推送（阶段 D）
)

// ntfyTarget ntfy 通道 target 的明文结构（整串 JSON 加密落库）。
type ntfyTarget struct {
	URL   string `json:"url"`   // 自建 ntfy 服务地址，如 https://ntfy.example.com
	Topic string `json:"topic"` // 订阅主题，如 qv-u1
	Token string `json:"token"` // 访问令牌 tk_xxx；可空（服务端开放匿名发布时）
}

// parseNtfyTarget 解析并校验 ntfy target JSON：url 必须 https、topic 非空。
func parseNtfyTarget(raw string) (ntfyTarget, error) {
	var t ntfyTarget
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return t, errors.New("ntfy 配置须为 JSON：{\"url\",\"topic\",\"token\"}")
	}
	t.URL = strings.TrimRight(strings.TrimSpace(t.URL), "/")
	t.Topic = strings.TrimSpace(t.Topic)
	t.Token = strings.TrimSpace(t.Token)
	u, err := url.Parse(t.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return t, errors.New("ntfy 服务地址非法（必须为 https）")
	}
	if t.Topic == "" {
		return t, errors.New("ntfy topic 不能为空")
	}
	return t, nil
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
		if in.Kind == model.NotifyKindNtfy {
			return errors.New("请填写 ntfy 服务地址与 topic")
		}
		return errors.New("请填写 sendkey 或 webhook 地址")
	}
	if in.Kind == model.NotifyKindWebhook && in.Target != "" {
		u, err := url.Parse(in.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("webhook 地址非法（仅支持 http/https）")
		}
	}
	if in.Kind == model.NotifyKindNtfy && in.Target != "" {
		if _, err := parseNtfyTarget(in.Target); err != nil {
			return err
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
	return s.sendTo(ch, NotifyMessage{
		Title:   "QuantVista 测试推送",
		Content: "这是一条来自 QuantVista 的测试消息，收到即表示通道配置正确。",
		Route:   "/", // ntfy 通道顺带验证点击跳转（SiteBaseURL 已配置时）
	})
}

// SendMsg 向用户所有启用的通道推送一条消息（best-effort，逐个通道独立成败）。
// 主入口：Route/Kind/Priority 由 ntfy 通道消费，老通道只用 Title/Content。
func (s *NotifyService) SendMsg(userID int64, msg NotifyMessage) {
	var rows []model.NotifyChannel
	if err := common.DB.Where("user_id = ? AND enabled = ?", userID, true).Find(&rows).Error; err != nil {
		return
	}
	for _, ch := range rows {
		_ = s.sendTo(ch, msg)
	}
}

// Send 纯文本推送（SendMsg 的薄包装，兼容旧调用方）。
func (s *NotifyService) Send(userID int64, title, content string) {
	s.SendMsg(userID, NotifyMessage{Title: title, Content: content})
}

// HasEnabledChannel 用户是否配置了启用的通道（供提醒评估判断是否需要推送）。
func (s *NotifyService) HasEnabledChannel(userID int64) bool {
	var cnt int64
	common.DB.Model(&model.NotifyChannel{}).Where("user_id = ? AND enabled = ?", userID, true).Count(&cnt)
	return cnt > 0
}

// sendTo 向单个通道发送，并回写 last_sent_at/last_error。
func (s *NotifyService) sendTo(ch model.NotifyChannel, msg NotifyMessage) error {
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
		err = sendServerChan(ctx, target, msg.Title, msg.Content)
	case model.NotifyKindWebhook:
		err = sendWebhook(ctx, target, msg.Title, msg.Content)
	case model.NotifyKindNtfy:
		err = sendNtfy(ctx, target, msg)
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

// ntfyKindTags NotifyMessage.Kind → ntfy tags（emoji shortcode，通知栏显示对应图标）。
var ntfyKindTags = map[string][]string{
	NotifyMsgKindAlert:  {"bell"},
	NotifyMsgKindEarn:   {"date"},
	NotifyMsgKindReport: {"newspaper"},
	NotifyMsgKindGuard:  {"shield"},
}

// buildNtfyPayload 构造 ntfy JSON 发布载荷（纯函数，便于单测）。
// click 仅在站点基础 URL 与 Route 均非空时下发；priority 仅在 1~5 时下发。
func buildNtfyPayload(t ntfyTarget, msg NotifyMessage, siteBaseURL string) map[string]any {
	payload := map[string]any{
		"topic":   t.Topic,
		"title":   msg.Title,
		"message": msg.Content,
	}
	if siteBaseURL != "" && msg.Route != "" {
		payload["click"] = strings.TrimRight(siteBaseURL, "/") + msg.Route
	}
	if msg.Priority >= 1 && msg.Priority <= 5 {
		payload["priority"] = msg.Priority
	}
	if tags, ok := ntfyKindTags[msg.Kind]; ok {
		payload["tags"] = tags
	}
	return payload
}

// sendNtfy POST JSON 到自建 ntfy 服务根路径（https://docs.ntfy.sh/publish/ Publish as JSON）。
// token 仅进 Authorization 头，绝不进错误信息/日志（错误只含 HTTP 状态与响应摘要）。
func sendNtfy(ctx context.Context, rawTarget string, msg NotifyMessage) error {
	t, err := parseNtfyTarget(rawTarget)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(buildNtfyPayload(t, msg, setting.SiteBaseURL()))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL+"/", bytes.NewReader(body))
	if err != nil {
		return errors.New("ntfy 请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	client := common.SafeHTTPClient(notifyTimeout, false) // 禁内网，防 SSRF（target 是公网 CF 域名）
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy HTTP %d: %s", resp.StatusCode, extractErr(raw))
	}
	return nil
}

func defaultChannelName(kind string) string {
	switch kind {
	case model.NotifyKindServerChan:
		return "Server酱"
	case model.NotifyKindNtfy:
		return "ntfy 推送"
	}
	return "Webhook"
}
