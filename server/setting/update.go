package setting

import (
	"errors"
	"net/url"
	"sort"
	"strconv"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpdateInput 是系统设置的部分更新；nil 保留原值，GitHub secret 空串也保留原值。
type UpdateInput struct {
	RegistrationOpen       *bool   `json:"registration_open"`
	GitHubOAuthEnabled     *bool   `json:"github_oauth_enabled"`
	GitHubClientID         *string `json:"github_client_id"`
	GitHubClientSecret     *string `json:"github_client_secret"`
	NewsCollectIntervalMin *int    `json:"news_collect_interval_min"`
	NewsAutoLLM            *bool   `json:"news_auto_llm"`
	LLMFallbackEnabled     *bool   `json:"llm_fallback_enabled"`
	LLMFallbackConfigID    *int64  `json:"llm_fallback_config_id"`
	LLMAccuracyContract    *bool   `json:"llm_accuracy_contract"`
	LLMEvidenceRefs        *bool   `json:"llm_evidence_refs"`
	LLMSemanticValidator   *bool   `json:"llm_semantic_validator"`
	LLMCapabilityRouting   *bool   `json:"llm_capability_routing"`
	LLMConditionalDebate   *bool   `json:"llm_conditional_debate"`
	LLMReflectionShadow    *bool   `json:"llm_reflection_shadow"`
	LLMChallenger          *bool   `json:"llm_challenger"`
	LLMLayeredContext      *bool   `json:"llm_layered_context"`
	LLMModelRouting        *bool   `json:"llm_model_routing"`
	SiteBaseURL            *string `json:"site_base_url"`
}

// Update 先校验全部字段，再原子持久化，最后一次性发布内存值。
// 单项 setter 同样复用此入口，避免并发设置出现数据库与内存的提交顺序倒置。
// LLM 配置归属由 service 层校验，setting 不反向依赖业务服务。
func Update(in UpdateInput) error {
	mu.Lock()
	defer mu.Unlock()
	values := map[string]string{}
	var publish []func()
	for _, field := range []struct {
		key    string
		input  *bool
		target *bool
	}{
		{keyRegistrationOpen, in.RegistrationOpen, &registrationOpen},
		{keyNewsAutoLLM, in.NewsAutoLLM, &newsAutoLLM},
		{keyLLMAccuracy, in.LLMAccuracyContract, &llmAccuracyContract},
		{keyLLMEvidenceRefs, in.LLMEvidenceRefs, &llmEvidenceRefs},
		{keyLLMSemanticValid, in.LLMSemanticValidator, &llmSemanticValidator},
		{keyLLMCapRouting, in.LLMCapabilityRouting, &llmCapabilityRouting},
		{keyLLMDebate, in.LLMConditionalDebate, &llmConditionalDebate},
		{keyLLMReflection, in.LLMReflectionShadow, &llmReflectionShadow},
		{keyLLMChallenger, in.LLMChallenger, &llmChallenger},
		{keyLLMLayeredContext, in.LLMLayeredContext, &llmLayeredContext},
		{keyLLMModelRouting, in.LLMModelRouting, &llmModelRouting},
	} {
		if field.input != nil {
			value := *field.input
			values[field.key] = strconv.FormatBool(value)
			publish = append(publish, func() { *field.target = value })
		}
	}
	if in.NewsCollectIntervalMin != nil {
		value := max(NewsIntervalMin, min(NewsIntervalMax, *in.NewsCollectIntervalMin))
		values[keyNewsInterval] = strconv.Itoa(value)
		publish = append(publish, func() { newsIntervalMin = value })
	}
	if in.SiteBaseURL != nil {
		value := normalizeSiteBaseURL(*in.SiteBaseURL)
		if value != "" {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return errors.New("站点基础 URL 非法（须为 http/https 完整地址）")
			}
		}
		values[keySiteBaseURL] = value
		publish = append(publish, func() { siteBaseURL = value })
	}
	if in.LLMFallbackEnabled != nil || in.LLMFallbackConfigID != nil {
		enabled, id := llmFallbackEnabled, llmFallbackID
		if in.LLMFallbackEnabled != nil {
			enabled = *in.LLMFallbackEnabled
		}
		if in.LLMFallbackConfigID != nil {
			id = max(int64(0), *in.LLMFallbackConfigID)
		}
		values[keyLLMFallbackEnabled], values[keyLLMFallbackID] = strconv.FormatBool(enabled), strconv.FormatInt(id, 10)
		publish = append(publish, func() { llmFallbackEnabled, llmFallbackID = enabled, id })
	}
	if in.GitHubOAuthEnabled != nil || in.GitHubClientID != nil || in.GitHubClientSecret != nil {
		enabled, id, secret := gitHubOAuthEnabled, gitHubClientID, gitHubClientSecret
		if in.GitHubOAuthEnabled != nil {
			enabled = *in.GitHubOAuthEnabled
		}
		if in.GitHubClientID != nil {
			id = *in.GitHubClientID
		}
		if in.GitHubClientSecret != nil && *in.GitHubClientSecret != "" {
			secret = *in.GitHubClientSecret
			cipher, err := common.Encrypt(secret)
			if err != nil {
				return err
			}
			values[keyGitHubClientSecret] = cipher
		}
		if enabled && id == "" {
			return errors.New("启用 GitHub 登录前必须配置 client_id")
		}
		if enabled && secret == "" {
			return errors.New("启用 GitHub 登录前必须配置 client_secret")
		}
		values[keyGitHubClientID], values[keyGitHubOAuthEnabled] = id, strconv.FormatBool(enabled)
		publish = append(publish, func() { gitHubOAuthEnabled, gitHubClientID, gitHubClientSecret = enabled, id, secret })
	}
	if len(values) == 0 {
		return nil
	}
	if common.DB == nil {
		return errors.New("数据库尚未初始化")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		for _, key := range keys {
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"}),
			}).Create(&model.Option{Key: key, Value: values[key]}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, apply := range publish {
		apply()
	}
	return nil
}
