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
	"regexp"
	"sort"
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
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	// ConfigID 仅 FetchModels 使用：拉取模型列表时若 APIKey 留空，用该配置已存的密钥
	// （覆盖「编辑时改了 Base URL 但不想重填密钥」）。增改路径忽略此字段。
	ConfigID        int64   `json:"config_id"`
	EndpointType    string  `json:"endpoint_type"` // chat_completions（默认）/ responses
	Temperature     float64 `json:"temperature"`
	MaxTokens       int     `json:"max_tokens"`
	ReasoningEffort string  `json:"reasoning_effort"` // 空=不发送该参数（沿用网关/模型默认档位）
	Stream          bool    `json:"stream"`
	IsDefault       bool    `json:"is_default"`
}

// llmProbeTarget 一次连接/能力探测的目标参数。探测链路（连通 → JSON 结构化 smoke →
// 思考档位）共用同一组参数且已达 9 项，收拢成结构体避免长签名（llmCallTarget 同款做法）。
type llmProbeTarget struct {
	UserID          int64
	ConfigID        int64
	Provider        string
	EndpointType    string
	BaseURL         string
	APIKey          string
	Model           string
	ReasoningEffort string
	AllowPrivate    bool
}

func (t llmProbeTarget) isResponses() bool {
	return normalizeEndpointType(t.EndpointType) == model.LLMEndpointResponses
}

func (t llmProbeTarget) sendsEffort() bool {
	return strings.TrimSpace(t.ReasoningEffort) != ""
}

// capabilityKey 该目标在能力观察存储中的 key。
func (t llmProbeTarget) capabilityKey() string {
	return llmCapabilityTarget(t.ConfigID, t.Provider, t.BaseURL, t.Model, t.EndpointType)
}

// chatMeta 探测调用的审计元数据（module=test，独立于业务 prompt）。
func (t llmProbeTarget) chatMeta() chatMeta {
	return chatMeta{CallerUserID: t.UserID, Module: "test", ConfigID: t.ConfigID, Provider: t.Provider}
}

// addEffort 按端点形态给探测 payload 写入思考档位（与业务侧 addReasoningEffortField 同形态）。
func (t llmProbeTarget) addEffort(payload map[string]any) {
	if !t.sendsEffort() {
		return
	}
	effort := strings.TrimSpace(t.ReasoningEffort)
	if t.isResponses() {
		payload["reasoning"] = map[string]any{"effort": effort}
		return
	}
	payload["reasoning_effort"] = effort
}

// normalizeEndpointType 空值归默认 chat_completions；非法值报错由 validate 负责。
func normalizeEndpointType(v string) string {
	if strings.TrimSpace(v) == "" {
		return model.LLMEndpointChat
	}
	return v
}

// llmReasoningEffortPattern 思考档位格式：小写字母数字与连字符/下划线。有意不做枚举白名单
// ——各家档位在持续扩（o 系列 low/medium/high、GPT-5 加 minimal、GPT-5.5 到 none/xhigh，
// 部分中转网关另有 max/ultra），写死枚举会让新档位必须改代码才能用。上游不认所配档位时
// 由请求层去参降级并落能力观察（looksLikeUnsupportedReasoningEffort）。
var llmReasoningEffortPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// defaultProviderName 新建配置的 provider 缺省值。表单已不再让用户选「类型」（原 openai/other
// 两项走的本就是同一条 OpenAI 兼容代码路径），但该字段在后端仍有实际语义——参与能力矩阵
// 初始声明（builtinProviderCapabilities）、审计标签与校准分层键，故保留并按兼容口径填 openai。
func defaultProviderName(v string) string {
	if p := strings.TrimSpace(v); p != "" {
		return p
	}
	return "openai"
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
	if in.MaxTokens < 1 {
		return errors.New("max_tokens 必须大于等于 1")
	}
	if effort := strings.TrimSpace(in.ReasoningEffort); effort != "" {
		if len(effort) > 16 || !llmReasoningEffortPattern.MatchString(effort) {
			return errors.New("推理档位只能是小写字母、数字、连字符或下划线（≤16 字符），留空表示不发送该参数")
		}
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
		UserID:          userID,
		Name:            in.Name,
		Provider:        defaultProviderName(in.Provider),
		BaseURL:         strings.TrimRight(in.BaseURL, "/"),
		APIKeyCipher:    cipher,
		Model:           in.Model,
		EndpointType:    normalizeEndpointType(in.EndpointType),
		Temperature:     in.Temperature,
		MaxTokens:       in.MaxTokens,
		ReasoningEffort: strings.TrimSpace(in.ReasoningEffort),
		Stream:          in.Stream,
		IsDefault:       in.IsDefault,
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
	// provider 已从表单撤除（只保留接口端点），入参空时沿用原值：它仍参与能力矩阵初始
	// 声明、审计标签与校准分层键，静默改写会让历史记录的这些维度漂移。
	if p := strings.TrimSpace(in.Provider); p != "" {
		cfg.Provider = p
	}
	cfg.BaseURL = strings.TrimRight(in.BaseURL, "/")
	cfg.Model = in.Model
	cfg.EndpointType = normalizeEndpointType(in.EndpointType)
	cfg.Temperature = in.Temperature
	cfg.MaxTokens = in.MaxTokens
	cfg.ReasoningEffort = strings.TrimSpace(in.ReasoningEffort)
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

// SetDefault 把指定配置设为默认（列表页一键操作，无需进编辑面板）。
// 与 Create/Update 同一原子纪律：置位与清其他默认在同一事务，中途失败不残留双默认。
func (s *LLMService) SetDefault(userID, id int64) (*LLMConfigView, error) {
	cfg, err := s.getOwned(userID, id)
	if err != nil {
		return nil, err
	}
	if !cfg.IsDefault {
		cfg.IsDefault = true
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(cfg).Update("is_default", true).Error; err != nil {
			return err
		}
		return clearOtherDefaultsTx(tx, userID, cfg.ID)
	})
	if err != nil {
		return nil, err
	}
	v := toView(*cfg)
	return &v, nil
}

// LLMModelOption 上游可用模型列表条目。
type LLMModelOption struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// llmModelsFetchLimit 单次返回的模型数上限：部分聚合中转的 /v1/models 会返回上百甚至上千项，
// 全量灌给前端下拉既慢也无用。超限时截断——调用方文案如实说明已截断。
const llmModelsFetchLimit = 500

// FetchModels 拉取上游可用模型列表（OpenAI 兼容 GET /v1/models）。
// 密钥三态：入参 APIKey 非空用它；留空且带 ConfigID 时解密该配置已存的密钥（覆盖
// 「编辑时改了 Base URL 但不想重填密钥」）；两者皆无则报错。
func (s *LLMService) FetchModels(userID int64, in LLMConfigInput, allowPrivate bool) ([]LLMModelOption, bool, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if baseURL == "" {
		return nil, false, errors.New("拉取模型需要先填写 Base URL")
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" && in.ConfigID > 0 {
		cfg, err := s.getOwned(userID, in.ConfigID)
		if err != nil {
			return nil, false, err
		}
		key, derr := common.Decrypt(cfg.APIKeyCipher)
		if derr != nil {
			return nil, false, errors.New("密钥解密失败")
		}
		apiKey = strings.TrimSpace(key)
		// 该配置本身是管理员的（普通用户不可能拿到别人的配置，getOwned 已限本人），
		// 内网放行判定沿用调用方角色，与测试连接一致。
		allowPrivate = llmAllowPrivate(allowPrivate, cfg)
	}
	if apiKey == "" {
		return nil, false, errors.New("拉取模型需要 API Key（新建配置请先填写，编辑已有配置可留空复用原密钥）")
	}

	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false, errors.New("Base URL 非法（仅支持 http/https）")
	}
	endpoint := modelsURL(baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := common.SafeHTTPClient(20*time.Second, allowPrivate) // 防 SSRF
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP %d%s：%s", resp.StatusCode, statusHint(resp.StatusCode), extractErr(raw))
	}
	var parsed struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &parsed); jerr != nil {
		// 与测试连接同款归因：SPA fallback / 网关拦截页会 200 + HTML。
		if looksLikeHTML(raw) {
			return nil, false, fmt.Errorf("服务返回了网页而非 API 响应：请检查 Base URL 是否为 API 地址（实际请求 %s）", endpoint)
		}
		return nil, false, fmt.Errorf("解析模型列表失败: %w（响应开头: %s）", jerr, bodySnippet(raw))
	}
	if len(parsed.Data) == 0 {
		return nil, false, fmt.Errorf("上游返回的模型列表为空（实际请求 %s）——该网关可能未开放 /v1/models，请勾选「自定义模型」手工填写", endpoint)
	}

	out := make([]LLMModelOption, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, LLMModelOption{ID: id, OwnedBy: strings.TrimSpace(m.OwnedBy)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	truncated := false
	if len(out) > llmModelsFetchLimit {
		out, truncated = out[:llmModelsFetchLimit], true
	}
	return out, truncated, nil
}

// modelsURL 由 Base URL 构造 /models 端点。与 chatCompletionsURL 同族惯例：根地址补
// /v1/models、以版本段结尾只补 /models、已是完整端点原样使用。
func modelsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/models") {
		return base
	}
	if endsWithVersionSegment(base) {
		return base + "/models"
	}
	return base + "/v1/models"
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
	return s.testConnection(llmProbeTarget{
		UserID: userID, ConfigID: cfg.ID, Provider: cfg.Provider, EndpointType: cfg.EndpointType,
		BaseURL: cfg.BaseURL, APIKey: key, Model: cfg.Model,
		ReasoningEffort: cfg.ReasoningEffort, AllowPrivate: allowPrivate,
	}), nil
}

// TestByInput 测试未保存的配置（前端表单即时测试）。
func (s *LLMService) TestByInput(userID int64, in LLMConfigInput, allowPrivate bool) (*TestResult, error) {
	if in.BaseURL == "" || in.Model == "" || in.APIKey == "" {
		return nil, errors.New("测试需要 base_url、model 与 api_key")
	}
	return s.testConnection(llmProbeTarget{
		UserID: userID, Provider: in.Provider, EndpointType: in.EndpointType,
		BaseURL: strings.TrimRight(in.BaseURL, "/"), APIKey: in.APIKey, Model: in.Model,
		ReasoningEffort: strings.TrimSpace(in.ReasoningEffort), AllowPrivate: allowPrivate,
	}), nil
}

// testConnection 目前仅实现 OpenAI 兼容口径（chat/completions 或 responses 最小请求）。
// 其他 provider（如 Anthropic 原生 /v1/messages）在此 switch 留口，后续按需补。
func (s *LLMService) testConnection(t llmProbeTarget) *TestResult {
	switch strings.ToLower(t.Provider) {
	default: // openai 及各类 OpenAI 兼容中转
		return s.testOpenAICompatibleForUser(t)
	}
}

// testOpenAICompatible 无配置身份的最小探测入口（测试与内部调用用）。
func (s *LLMService) testOpenAICompatible(endpointType, baseURL, apiKey, modelName string, allowPrivate bool) *TestResult {
	return s.testOpenAICompatibleForUser(llmProbeTarget{
		Provider: "openai", EndpointType: endpointType, BaseURL: baseURL,
		APIKey: apiKey, Model: modelName, AllowPrivate: allowPrivate,
	})
}

// buildProbeBody 最小连通探测请求体。16 对齐 new-api 的渠道测试请求；过小部分上游会拒绝
// 或回空（推理模型尤甚）。带思考档位时按端点形态注入——测试必须与业务请求同形态，
// 否则「测试通过 = 实际可用」的口径破裂。
func buildProbeBody(t llmProbeTarget) []byte {
	payload := map[string]any{"model": t.Model, "stream": false}
	if t.isResponses() {
		payload["input"] = []map[string]string{{"role": "user", "content": "hi"}}
		payload["max_output_tokens"] = 16
	} else {
		payload["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
		payload["max_tokens"] = 16
	}
	t.addEffort(payload)
	body, _ := json.Marshal(payload)
	return body
}

func (s *LLMService) testOpenAICompatibleForUser(t llmProbeTarget) (result *TestResult) {
	started := time.Now()
	params := chatParams{
		Model: t.Model, EndpointType: t.EndpointType, ReasoningEffort: t.ReasoningEffort,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     t.chatMeta(),
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
	u, err := url.Parse(t.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &TestResult{OK: false, Message: "Base URL 非法（仅支持 http/https）"}
	}

	// 与真实分析调用（ai_client.go doChat/doResponses）同一拼接逻辑：测试通过 = 实际可用。
	isResponses := t.isResponses()
	endpoint := chatCompletionsURL(t.BaseURL)
	if isResponses {
		endpoint = responsesURL(t.BaseURL)
	}

	client := common.SafeHTTPClient(20*time.Second, t.AllowPrivate) // 防 SSRF（管理员可放行内网自建模型）
	send := func(body []byte) (int, []byte, int64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if rerr != nil {
			return 0, nil, 0, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
		start := time.Now()
		res, derr := client.Do(req)
		latency := time.Since(start).Milliseconds()
		if derr != nil {
			return 0, nil, latency, derr
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return res.StatusCode, raw, latency, nil
	}

	status, raw, latency, err := send(buildProbeBody(t))
	if err != nil {
		return &TestResult{OK: false, LatencyMs: latency, Message: "请求失败: " + err.Error()}
	}
	// 思考档位被拒（参数不认或档位不在取值集合内）：去参重试一次，让测试反映真实可用性
	// ——业务调用同样会自动去参降级，此处若直接报失败，用户会误以为 URL/密钥配错了。
	configuredEffort, effortRejected := t.ReasoningEffort, false
	if t.sendsEffort() && looksLikeUnsupportedReasoningEffort(status, raw) {
		effortRejected = true
		reason := fmt.Sprintf("连接测试 HTTP %d 拒绝思考档位 %q", status, configuredEffort)
		t.ReasoningEffort = ""
		params.ReasoningEffort = ""
		status, raw, latency, err = send(buildProbeBody(t))
		if err != nil {
			return &TestResult{OK: false, LatencyMs: latency, Message: "请求失败: " + err.Error()}
		}
		if status == http.StatusOK {
			// 去参后成功才证明失败确实源于该参数（与业务侧 capConfirms 同一纪律）。
			observeLLMCapability(t.capabilityKey(), capReasoningEffort, capUnsupported, reason)
		}
	}

	if status != http.StatusOK {
		return &TestResult{
			OK:        false,
			LatencyMs: latency,
			Message:   fmt.Sprintf("HTTP %d: %s", status, extractErr(raw)),
		}
	}
	// 200 也要能解析出对应端点的结构才算通过——SPA fallback / 网关拦截页会 200 + HTML，
	// 只看状态码会"测试成功、实际分析失败"（json: invalid character '<'）。
	capTarget := t.capabilityKey()
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
			observeLLMCapability(capTarget, capEndpointResponses, capUnsupported, "连通但响应不含 output")
			return &TestResult{OK: false, LatencyMs: latency,
				Message: "连通但响应不含 output（" + extractErr(raw) + "），可能不支持 Responses 端点"}
		}
		observeLLMCapability(capTarget, capEndpointResponses, capSupported, "连通探测成功")
		return &TestResult{OK: true, LatencyMs: latency,
			Message: "连接成功" + s.jsonModeSmokeNote(t) + effortNote(configuredEffort, effortRejected)}
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
		observeLLMCapability(capTarget, capEndpointChat, capUnsupported, "连通但响应不含 choices")
		return &TestResult{OK: false, LatencyMs: latency,
			Message: "连通但响应不含 choices（" + extractErr(raw) + "），可能不是 OpenAI 兼容接口"}
	}
	observeLLMCapability(capTarget, capEndpointChat, capSupported, "连通探测成功")
	return &TestResult{OK: true, LatencyMs: latency,
		Message: "连接成功" + s.jsonModeSmokeNote(t) + effortNote(configuredEffort, effortRejected)}
}

// effortNote 思考档位探测结论附注。**未配档位时无附注**，保持旧文案不变。
// 这是用户判断「所配档位到底有没有生效」的主入口：被拒时业务调用会静默去参降级，
// 不在这里如实说明，用户会以为档位生效了、实际一直在用网关默认档位。
func effortNote(effort string, rejected bool) string {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ""
	}
	if rejected {
		return fmt.Sprintf("；推理档位 %s：上游不接受（已自动去参重试，实际使用网关默认档位——请按上游提示改配档位）", effort)
	}
	return fmt.Sprintf("；推理档位 %s：已接受", effort)
}

// jsonModeSmokeNote 运行 JSON 结构化能力 smoke 并生成测试结果附注（P0-5）。
func (s *LLMService) jsonModeSmokeNote(t llmProbeTarget) string {
	switch s.probeJSONModeCapability(t) {
	case capSupported:
		return "；JSON 结构化：支持"
	case capUnsupported:
		return "；JSON 结构化：不支持（已记录，业务结构化调用将直接按纯文本请求并靠提示词约束）"
	default:
		return "；JSON 结构化：未能确认（业务调用将按需在线回落）"
	}
}

// probeJSONModeCapability JSON 结构化能力 smoke（P0-5 capability matrix）：基础连通探测
// 成功后追加一次带 response_format/text.format 的最小探测请求，结论写入能力观察存储，
// 供业务调用的声明化路由（applyCapabilityRouting）消费——与四处隐式回落点同一观察口径。
// 仍是独立 capability probe：module=test 单独审计、不注入任何业务 prompt；
// 探测通过（json_object supported）不代表业务推理可用。
// 入参 t 的思考档位已由连通探测阶段校准（被拒则已清空），此处不再重复该维度的判定。
func (s *LLMService) probeJSONModeCapability(t llmProbeTarget) llmCapState {
	const probeMsg = `请只输出一个 JSON 对象：{"ok":true}`
	isResponses := t.isResponses()
	var endpoint string
	payload := map[string]any{"model": t.Model, "stream": false}
	if isResponses {
		endpoint = responsesURL(t.BaseURL)
		payload["input"] = []map[string]string{{"role": "user", "content": probeMsg}}
		payload["max_output_tokens"] = 32
		payload["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
	} else {
		endpoint = chatCompletionsURL(t.BaseURL)
		payload["messages"] = []map[string]string{{"role": "user", "content": probeMsg}}
		payload["max_tokens"] = 32
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	t.addEffort(payload)
	body, _ := json.Marshal(payload)
	params := chatParams{
		Model: t.Model, EndpointType: t.EndpointType, JSONMode: true, ReasoningEffort: t.ReasoningEffort,
		Messages: []chatMessage{{Role: "user", Content: probeMsg}},
		Meta:     t.chatMeta(),
	}
	started := time.Now()
	var probeRes *chatResult
	var probeErr error
	defer func() { writeLLMCallLog(params, false, probeRes, probeErr, time.Since(started)) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		probeErr = err
		return capUnknown
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	client := common.SafeHTTPClient(20*time.Second, t.AllowPrivate)
	resp, err := client.Do(req)
	if err != nil {
		probeErr = err
		return capUnknown
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	target := t.capabilityKey()
	if resp.StatusCode == http.StatusOK {
		hasContent := false
		if isResponses {
			var parsed struct {
				Output []json.RawMessage `json:"output"`
			}
			hasContent = json.Unmarshal(raw, &parsed) == nil && len(parsed.Output) > 0
		} else {
			var parsed struct {
				Choices []json.RawMessage `json:"choices"`
			}
			hasContent = json.Unmarshal(raw, &parsed) == nil && len(parsed.Choices) > 0
		}
		if hasContent {
			observeLLMCapability(target, capJSONObject, capSupported, "provider smoke 结构化探测成功")
			probeRes = &chatResult{Content: "json_object supported"}
			return capSupported
		}
		probeErr = errors.New("结构化探测 200 但响应无内容")
		return capUnknown
	}
	if looksLikeUnsupportedJSONMode(resp.StatusCode, raw) {
		// 判定确证性说明（对齐业务侧「fallback 成功才提交观察」纪律）：smoke 在基础连通
		// 探测 200 之后才运行——同目标不带结构化参数的请求已确认成功，此处带参数 4xx 且
		// 文案明确指向结构化字段，构成同样的对照证据；泛 4xx（模型名/鉴权等）不落观察。
		observeLLMCapability(target, capJSONObject, capUnsupported,
			fmt.Sprintf("provider smoke HTTP %d 拒绝结构化参数", resp.StatusCode))
		probeRes = &chatResult{Content: "json_object unsupported"}
		return capUnsupported
	}
	// 网络/限流/5xx 等非结论性失败：不落观察（unknown 保持乐观路径 + 隐式回落兜底）。
	probeErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, extractErr(raw))
	return capUnknown
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
// 失败统一挂机读码 llm_unavailable（文案原样保留），供 API 包络 code 字段透出。
func (s *LLMService) ResolveForUse(userID, id int64) (*model.LLMConfig, string, error) {
	cfg, key, err := s.resolveForUseInner(userID, id)
	if err != nil {
		return nil, "", asLLMUnavailable(err)
	}
	return cfg, key, nil
}

func (s *LLMService) resolveForUseInner(userID, id int64) (*model.LLMConfig, string, error) {
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

// asLLMUnavailable 将配置解析失败挂成机读拒答码；已是 RefusalError 则原样返回。
func asLLMUnavailable(err error) error {
	if err == nil {
		return nil
	}
	if RefusalCodeOf(err) != "" {
		return err
	}
	return refusalErr(RefusalLLMUnavailable, err.Error())
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

// isEnabledAdmin 是否为启用状态的管理员。保留该语义别名，供回退配置所有者的
// 合法性判断使用。
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
