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

	"gorm.io/gorm"
)

type LLMService struct{}

func NewLLMService() *LLMService { return &LLMService{} }

// LLMConfigView 返回给前端的视图：内嵌配置（APIKeyCipher 已 json:"-" 不输出）+ 是否已设密钥。
type LLMConfigView struct {
	model.LLMConfig
	HasAPIKey bool `json:"has_api_key"`
}

// LLMConfigInput 增改入参。APIKey 为明文；更新时留空表示保留原密钥。
type LLMConfigInput struct {
	Name         string  `json:"name"`
	Provider     string  `json:"provider"`
	BaseURL      string  `json:"base_url"`
	APIKey       string  `json:"api_key"`
	Model        string  `json:"model"`
	EndpointType string  `json:"endpoint_type"` // chat_completions（默认）/ responses
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"max_tokens"`
	Stream       bool    `json:"stream"`
	IsDefault    bool    `json:"is_default"`
}

// normalizeEndpointType 空值归默认 chat_completions；非法值报错由 validate 负责。
func normalizeEndpointType(v string) string {
	if strings.TrimSpace(v) == "" {
		return model.LLMEndpointChat
	}
	return v
}

func toView(cfg model.LLMConfig) LLMConfigView {
	v := LLMConfigView{LLMConfig: cfg, HasAPIKey: cfg.APIKeyCipher != ""}
	v.APIKeyCipher = "" // 双保险，绝不外泄
	return v
}

// List 列出用户的 LLM 配置。
func (s *LLMService) List(userID int64) ([]LLMConfigView, error) {
	var rows []model.LLMConfig
	if err := common.DB.Where("user_id = ?", userID).Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LLMConfigView, 0, len(rows))
	for _, r := range rows {
		out = append(out, toView(r))
	}
	return out, nil
}

func (s *LLMService) validate(in LLMConfigInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("名称不能为空")
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		return errors.New("Base URL 不能为空")
	}
	if strings.TrimSpace(in.Model) == "" {
		return errors.New("模型名不能为空")
	}
	if in.Temperature < 0 || in.Temperature > 2 {
		return errors.New("temperature 需在 0~2 之间")
	}
	if in.MaxTokens < 1 || in.MaxTokens > 200000 {
		return errors.New("max_tokens 取值不合理")
	}
	switch normalizeEndpointType(in.EndpointType) {
	case model.LLMEndpointChat, model.LLMEndpointResponses:
	default:
		return errors.New("端点类型仅支持 chat_completions / responses")
	}
	return nil
}

// Create 新建配置。API Key 加密落库。
func (s *LLMService) Create(userID int64, in LLMConfigInput) (*LLMConfigView, error) {
	if err := s.validate(in); err != nil {
		return nil, err
	}
	cipher, err := common.Encrypt(in.APIKey)
	if err != nil {
		return nil, fmt.Errorf("API Key 加密失败: %w", err)
	}
	cfg := model.LLMConfig{
		UserID:       userID,
		Name:         in.Name,
		Provider:     in.Provider,
		BaseURL:      strings.TrimRight(in.BaseURL, "/"),
		APIKeyCipher: cipher,
		Model:        in.Model,
		EndpointType: normalizeEndpointType(in.EndpointType),
		Temperature:  in.Temperature,
		MaxTokens:    in.MaxTokens,
		Stream:       in.Stream,
		IsDefault:    in.IsDefault,
	}
	// 设默认与清其他默认同一事务：中途失败不残留双默认。
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cfg).Error; err != nil {
			return err
		}
		if cfg.IsDefault {
			return clearOtherDefaultsTx(tx, userID, cfg.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	v := toView(cfg)
	return &v, nil
}

// Update 更新配置。APIKey 留空则保留原密钥。
func (s *LLMService) Update(userID, id int64, in LLMConfigInput) (*LLMConfigView, error) {
	if err := s.validate(in); err != nil {
		return nil, err
	}
	cfg, err := s.getOwned(userID, id)
	if err != nil {
		return nil, err
	}
	cfg.Name = in.Name
	cfg.Provider = in.Provider
	cfg.BaseURL = strings.TrimRight(in.BaseURL, "/")
	cfg.Model = in.Model
	cfg.EndpointType = normalizeEndpointType(in.EndpointType)
	cfg.Temperature = in.Temperature
	cfg.MaxTokens = in.MaxTokens
	cfg.Stream = in.Stream
	cfg.IsDefault = in.IsDefault
	if in.APIKey != "" {
		cipher, err := common.Encrypt(in.APIKey)
		if err != nil {
			return nil, fmt.Errorf("API Key 加密失败: %w", err)
		}
		cfg.APIKeyCipher = cipher
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(cfg).Error; err != nil {
			return err
		}
		if cfg.IsDefault {
			return clearOtherDefaultsTx(tx, userID, cfg.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	v := toView(*cfg)
	return &v, nil
}

// Delete 删除配置。
func (s *LLMService) Delete(userID, id int64) error {
	res := common.DB.Where("user_id = ? AND id = ?", userID, id).Delete(&model.LLMConfig{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("配置不存在")
	}
	return nil
}

// TestResult 测试连接结果。
type TestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

// TestByID 测试已保存配置（解密存储的密钥）。allowPrivate 由调用方按角色决定（管理员放行内网）。
func (s *LLMService) TestByID(userID, id int64, allowPrivate bool) (*TestResult, error) {
	cfg, err := s.getOwned(userID, id)
	if err != nil {
		return nil, err
	}
	key, err := common.Decrypt(cfg.APIKeyCipher)
	if err != nil {
		return nil, errors.New("密钥解密失败")
	}
	return s.testConnection(userID, cfg.ID, cfg.Provider, cfg.EndpointType, cfg.BaseURL, key, cfg.Model, allowPrivate), nil
}

// TestByInput 测试未保存的配置（前端表单即时测试）。
func (s *LLMService) TestByInput(userID int64, in LLMConfigInput, allowPrivate bool) (*TestResult, error) {
	if in.BaseURL == "" || in.Model == "" || in.APIKey == "" {
		return nil, errors.New("测试需要 base_url、model 与 api_key")
	}
	return s.testConnection(userID, 0, in.Provider, in.EndpointType, strings.TrimRight(in.BaseURL, "/"), in.APIKey, in.Model, allowPrivate), nil
}

// testConnection 目前仅实现 OpenAI 兼容口径（chat/completions 或 responses 最小请求）。
// 其他 provider（如 Anthropic 原生 /v1/messages）在此 switch 留口，后续按需补。
func (s *LLMService) testConnection(userID, configID int64, provider, endpointType, baseURL, apiKey, modelName string, allowPrivate bool) *TestResult {
	switch strings.ToLower(provider) {
	default: // openai 及各类 OpenAI 兼容中转
		return s.testOpenAICompatibleForUser(userID, configID, provider, endpointType, baseURL, apiKey, modelName, allowPrivate)
	}
}

func (s *LLMService) testOpenAICompatible(endpointType, baseURL, apiKey, modelName string, allowPrivate bool) *TestResult {
	return s.testOpenAICompatibleForUser(0, 0, "openai", endpointType, baseURL, apiKey, modelName, allowPrivate)
}

func (s *LLMService) testOpenAICompatibleForUser(userID, configID int64, provider, endpointType, baseURL, apiKey, modelName string, allowPrivate bool) (result *TestResult) {
	started := time.Now()
	params := chatParams{
		Model: modelName, EndpointType: endpointType,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     chatMeta{CallerUserID: userID, Module: "test", ConfigID: configID, Provider: provider},
	}
	defer func() {
		var res *chatResult
		var callErr error
		if result != nil && result.OK {
			res = &chatResult{Content: result.Message, LatencyMs: result.LatencyMs}
		} else if result != nil {
			callErr = errors.New(result.Message)
		}
		writeLLMCallLog(params, false, res, callErr, time.Since(started))
	}()
	// 校验 scheme：仅允许 http/https，防 file://、gopher:// 等被利用。
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &TestResult{OK: false, Message: "Base URL 非法（仅支持 http/https）"}
	}

	// 与真实分析调用（ai_client.go doChat/doResponses）同一拼接逻辑：测试通过 = 实际可用。
	isResponses := normalizeEndpointType(endpointType) == model.LLMEndpointResponses
	var endpoint string
	var body []byte
	if isResponses {
		endpoint = responsesURL(baseURL)
		body, _ = json.Marshal(map[string]any{
			"model": modelName,
			"input": []map[string]string{{"role": "user", "content": "hi"}},
			// 16 对齐 new-api 的渠道测试请求；过小部分上游会拒绝或回空（推理模型尤甚）。
			"max_output_tokens": 16,
			"stream":            false,
		})
	} else {
		endpoint = chatCompletionsURL(baseURL)
		body, _ = json.Marshal(map[string]any{
			"model":      modelName,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 16,
			"stream":     false,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &TestResult{OK: false, Message: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := common.SafeHTTPClient(20*time.Second, allowPrivate) // 防 SSRF（管理员可放行内网自建模型）
	start := time.Now()
	res, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &TestResult{OK: false, LatencyMs: latency, Message: "请求失败: " + err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))

	if res.StatusCode != http.StatusOK {
		return &TestResult{
			OK:        false,
			LatencyMs: latency,
			Message:   fmt.Sprintf("HTTP %d: %s", res.StatusCode, extractErr(raw)),
		}
	}
	// 200 也要能解析出对应端点的结构才算通过——SPA fallback / 网关拦截页会 200 + HTML，
	// 只看状态码会"测试成功、实际分析失败"（json: invalid character '<'）。
	if isResponses {
		var parsed struct {
			Output []json.RawMessage `json:"output"`
		}
		if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
			msg := "服务返回的不是 JSON"
			if looksLikeHTML(raw) {
				msg = "服务返回了网页而非 API 响应"
			}
			return &TestResult{OK: false, LatencyMs: latency,
				Message: fmt.Sprintf("%s：请检查 Base URL 是否为 API 地址（实际请求 %s，根地址会自动补 /v1/responses）", msg, endpoint)}
		}
		if len(parsed.Output) == 0 {
			return &TestResult{OK: false, LatencyMs: latency,
				Message: "连通但响应不含 output（" + extractErr(raw) + "），可能不支持 Responses 端点"}
		}
		return &TestResult{OK: true, LatencyMs: latency, Message: "连接成功"}
	}
	var parsed struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
		msg := "服务返回的不是 JSON"
		if looksLikeHTML(raw) {
			msg = "服务返回了网页而非 API 响应"
		}
		return &TestResult{OK: false, LatencyMs: latency,
			Message: fmt.Sprintf("%s：请检查 Base URL 是否为 API 地址（实际请求 %s，根地址会自动补 /v1/chat/completions）", msg, endpoint)}
	}
	if len(parsed.Choices) == 0 {
		return &TestResult{OK: false, LatencyMs: latency,
			Message: "连通但响应不含 choices（" + extractErr(raw) + "），可能不是 OpenAI 兼容接口"}
	}
	return &TestResult{OK: true, LatencyMs: latency, Message: "连接成功"}
}

// extractErr 从上游错误体里抽取 message：兼容 OpenAI 风格 {"error":{"message":...}}、
// error 为裸字符串、以及各类网关的顶层 message/msg/error_msg/detail 字段
// （new-api GeneralErrorResponse 同款宽容解析）；全抽不到则返回截断原文。
func extractErr(raw []byte) string {
	var generic struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		ErrMsg  string          `json:"error_msg"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(raw, &generic) == nil {
		if m := errorMessageFromRaw(generic.Error); m != "" {
			return m
		}
		for _, v := range []string{generic.Message, generic.Msg, generic.ErrMsg, generic.Detail} {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	if looksLikeHTML(raw) {
		return "返回了 HTML 页面而非 API 响应（通常是 Base URL 路径不对或被网关拦截）"
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "无响应内容"
	}
	return s
}

func (s *LLMService) getOwned(userID, id int64) (*model.LLMConfig, error) {
	var cfg model.LLMConfig
	err := common.DB.Where("user_id = ? AND id = ?", userID, id).First(&cfg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("配置不存在")
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ResolveForUse 取一份可用于实际调用的 LLM 配置并解密密钥。
// id>0 取指定配置（限本人）；id<=0 取默认配置（无默认则取最早一条）。
// 本人一条配置都没有时，回退到首个启用管理员的默认配置——管理员代付 key，
// 次数/token 配额仍按发起用户记（consumeQuota 在各调用方按发起 userID）。
func (s *LLMService) ResolveForUse(userID, id int64) (*model.LLMConfig, string, error) {
	var cfg model.LLMConfig
	if id > 0 {
		c, err := s.getOwned(userID, id)
		if err != nil {
			return nil, "", err
		}
		cfg = *c
	} else {
		err := common.DB.Where("user_id = ?", userID).
			Order("is_default DESC, id ASC").First(&cfg).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = s.adminFallback(userID, &cfg)
		}
		if err != nil {
			return nil, "", err
		}
	}
	key, err := common.Decrypt(cfg.APIKeyCipher)
	if err != nil {
		return nil, "", errors.New("密钥解密失败")
	}
	if strings.TrimSpace(key) == "" {
		return nil, "", errors.New("该 LLM 配置缺少 API Key，请先补全")
	}
	return &cfg, key, nil
}

// adminFallback 无自有配置时的用户回退入口：受管理后台"LLM 回退"开关控制，
// 关闭时保持"请先在设置中添加"的引导语义；发起者本人就是候选管理员时同样引导
// （自己都没配置，回退到自己没有意义）。
func (s *LLMService) adminFallback(userID int64, cfg *model.LLMConfig) error {
	errGuide := errors.New("尚未配置任何 LLM，请先在设置中添加")
	if !setting.LLMFallbackEnabled() {
		return errGuide
	}
	if err := resolveSystemFallbackConfig(cfg); err != nil || cfg.UserID == userID {
		return errGuide
	}
	return nil
}

// resolveSystemFallbackConfig 解析"系统默认 LLM"：管理后台指定的回退配置优先
// （须仍存在且所有者是启用管理员，失效则静默回落），否则取首个启用管理员的默认配置。
// 供用户回退（adminFallback）与新闻情绪分析（resolveNewsLLM）共用，不受回退开关控制。
func resolveSystemFallbackConfig(cfg *model.LLMConfig) error {
	if id := setting.LLMFallbackConfigID(); id > 0 {
		var c model.LLMConfig
		if err := common.DB.First(&c, id).Error; err == nil && isEnabledAdmin(c.UserID) {
			*cfg = c
			return nil
		}
		// 指定配置已删/所有者被禁用或降级：回落自动逻辑，不让系统 AI 能力瘫在死引用上。
	}
	adminID, err := firstEnabledAdminID()
	if err != nil {
		return err
	}
	err = common.DB.Where("user_id = ?", adminID).
		Order("is_default DESC, id ASC").First(cfg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errors.New("管理员尚未配置默认 LLM")
	}
	return err
}

// firstEnabledAdminID 首个启用状态的管理员 ID。
func firstEnabledAdminID() (int64, error) {
	var admin model.User
	if err := common.DB.Select("id").Where("role = ? AND status = ?", model.RoleAdmin, model.StatusEnabled).
		Order("id ASC").First(&admin).Error; err != nil {
		return 0, errors.New("无可用管理员账号")
	}
	return admin.ID, nil
}

// isEnabledAdmin 是否为启用状态的管理员（isAdminUser 只看角色，这里连状态一起校验，
// 供回退配置的所有者合法性判断——被禁用管理员的配置不应继续被系统使用）。
func isEnabledAdmin(userID int64) bool {
	var u model.User
	if err := common.DB.Select("role, status").First(&u, userID).Error; err != nil {
		return false
	}
	return u.Role == model.RoleAdmin && u.Status == model.StatusEnabled
}

// llmAllowPrivate 内网地址放行判定：发起者是管理员，或配置本身属于管理员
// （普通用户回退用管理员配置时，内网 URL 是管理员配的、非用户可控输入，放行安全）。
func llmAllowPrivate(callerAllow bool, cfg *model.LLMConfig) bool {
	if callerAllow {
		return true
	}
	return cfg != nil && isAdminUser(cfg.UserID)
}

// clearOtherDefaultsTx 事务内把该用户其余配置的 is_default 清掉（与设默认原子执行）。
func clearOtherDefaultsTx(tx *gorm.DB, userID, keepID int64) error {
	return tx.Model(&model.LLMConfig{}).
		Where("user_id = ? AND id <> ?", userID, keepID).
		Update("is_default", false).Error
}
