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
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	Stream      bool    `json:"stream"`
	IsDefault   bool    `json:"is_default"`
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
		Temperature:  in.Temperature,
		MaxTokens:    in.MaxTokens,
		Stream:       in.Stream,
		IsDefault:    in.IsDefault,
	}
	if err := common.DB.Create(&cfg).Error; err != nil {
		return nil, err
	}
	if cfg.IsDefault {
		s.clearOtherDefaults(userID, cfg.ID)
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
	if err := common.DB.Save(cfg).Error; err != nil {
		return nil, err
	}
	if cfg.IsDefault {
		s.clearOtherDefaults(userID, cfg.ID)
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
	return s.testConnection(cfg.Provider, cfg.BaseURL, key, cfg.Model, allowPrivate), nil
}

// TestByInput 测试未保存的配置（前端表单即时测试）。
func (s *LLMService) TestByInput(in LLMConfigInput, allowPrivate bool) (*TestResult, error) {
	if in.BaseURL == "" || in.Model == "" || in.APIKey == "" {
		return nil, errors.New("测试需要 base_url、model 与 api_key")
	}
	return s.testConnection(in.Provider, strings.TrimRight(in.BaseURL, "/"), in.APIKey, in.Model, allowPrivate), nil
}

// testConnection 目前仅实现 OpenAI 兼容口径（/chat/completions 最小请求）。
// 其他 provider（如 Anthropic 原生 /v1/messages）在此 switch 留口，后续按需补。
func (s *LLMService) testConnection(provider, baseURL, apiKey, modelName string, allowPrivate bool) *TestResult {
	switch strings.ToLower(provider) {
	default: // openai 及各类 OpenAI 兼容中转
		return s.testOpenAICompatible(baseURL, apiKey, modelName, allowPrivate)
	}
}

func (s *LLMService) testOpenAICompatible(baseURL, apiKey, modelName string, allowPrivate bool) *TestResult {
	// 校验 scheme：仅允许 http/https，防 file://、gopher:// 等被利用。
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &TestResult{OK: false, Message: "Base URL 非法（仅支持 http/https）"}
	}

	endpoint := baseURL
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	body, _ := json.Marshal(map[string]any{
		"model":      modelName,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})

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
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))

	if res.StatusCode == http.StatusOK {
		return &TestResult{OK: true, LatencyMs: latency, Message: "连接成功"}
	}
	return &TestResult{
		OK:        false,
		LatencyMs: latency,
		Message:   fmt.Sprintf("HTTP %d: %s", res.StatusCode, extractErr(raw)),
	}
}

// extractErr 从 OpenAI 风格错误体里抽取 message，抽不到则返回截断原文。
func extractErr(raw []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
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
// id>0 取指定配置；id<=0 取默认配置（无默认则取最早一条）。均限本人。
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
			return nil, "", errors.New("尚未配置任何 LLM，请先在设置中添加")
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

func (s *LLMService) clearOtherDefaults(userID, keepID int64) {
	common.DB.Model(&model.LLMConfig{}).
		Where("user_id = ? AND id <> ?", userID, keepID).
		Update("is_default", false)
}
