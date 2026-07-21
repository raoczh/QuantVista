package service

import (
	"errors"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm"
)

type AdminService struct{}

func NewAdminService() *AdminService { return &AdminService{} }

// SystemSettingsView 后台系统设置视图（不泄露 GitHub secret 本身）。
type SystemSettingsView struct {
	RegistrationOpen       bool   `json:"registration_open"`
	GitHubOAuthEnabled     bool   `json:"github_oauth_enabled"`
	GitHubClientID         string `json:"github_client_id"`
	HasGitHubSecret        bool   `json:"has_github_secret"`
	NewsCollectIntervalMin int    `json:"news_collect_interval_min"`
	NewsAutoLLM            bool   `json:"news_auto_llm"`
	LLMFallbackEnabled     bool   `json:"llm_fallback_enabled"`
	LLMFallbackConfigID    int64  `json:"llm_fallback_config_id"`
	LLMAccuracyContract    bool   `json:"llm_accuracy_contract"`
	LLMEvidenceRefs        bool   `json:"llm_evidence_refs"`
	LLMSemanticValidator   bool   `json:"llm_semantic_validator"`
	SiteBaseURL            string `json:"site_base_url"`
}

// GetSettings 读取当前系统设置。
func (s *AdminService) GetSettings() SystemSettingsView {
	return SystemSettingsView{
		RegistrationOpen:       setting.RegistrationOpen(),
		GitHubOAuthEnabled:     setting.GitHubOAuthEnabled(),
		GitHubClientID:         setting.GitHubClientID(),
		HasGitHubSecret:        setting.HasGitHubSecret(),
		NewsCollectIntervalMin: setting.NewsCollectIntervalMin(),
		NewsAutoLLM:            setting.NewsAutoLLM(),
		LLMFallbackEnabled:     setting.LLMFallbackEnabled(),
		LLMFallbackConfigID:    setting.LLMFallbackConfigID(),
		LLMAccuracyContract:    setting.LLMAccuracyContract(),
		LLMEvidenceRefs:        setting.LLMEvidenceRefs(),
		LLMSemanticValidator:   setting.LLMSemanticValidator(),
		SiteBaseURL:            setting.SiteBaseURL(),
	}
}

// UpdateSettingsInput 部分更新；指针为 nil 表示该项不变。GitHubClientSecret 为空字符串表示保留原值。
type UpdateSettingsInput struct {
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
	SiteBaseURL            *string `json:"site_base_url"` // 空串 = 清除（推送通知不带点击跳转）
}

// UpdateSettings 应用系统设置变更。
func (s *AdminService) UpdateSettings(in UpdateSettingsInput) (SystemSettingsView, error) {
	if in.RegistrationOpen != nil {
		if err := setting.SetRegistrationOpen(*in.RegistrationOpen); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.NewsCollectIntervalMin != nil {
		if err := setting.SetNewsCollectIntervalMin(*in.NewsCollectIntervalMin); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.NewsAutoLLM != nil {
		if err := setting.SetNewsAutoLLM(*in.NewsAutoLLM); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.LLMAccuracyContract != nil {
		if err := setting.SetLLMAccuracyContract(*in.LLMAccuracyContract); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.LLMEvidenceRefs != nil {
		if err := setting.SetLLMEvidenceRefs(*in.LLMEvidenceRefs); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.LLMSemanticValidator != nil {
		if err := setting.SetLLMSemanticValidator(*in.LLMSemanticValidator); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.SiteBaseURL != nil {
		if err := setting.SetSiteBaseURL(*in.SiteBaseURL); err != nil {
			return SystemSettingsView{}, err
		}
	}
	if in.LLMFallbackEnabled != nil || in.LLMFallbackConfigID != nil {
		enabled := setting.LLMFallbackEnabled()
		if in.LLMFallbackEnabled != nil {
			enabled = *in.LLMFallbackEnabled
		}
		configID := setting.LLMFallbackConfigID()
		if in.LLMFallbackConfigID != nil {
			configID = *in.LLMFallbackConfigID
		}
		// 指定配置必须真实存在且所有者是启用管理员——防手工 PUT 塞进普通用户/已删配置的 id。
		if configID > 0 {
			var cfg model.LLMConfig
			if err := common.DB.Select("id, user_id").First(&cfg, configID).Error; err != nil {
				return SystemSettingsView{}, errors.New("指定的回退 LLM 配置不存在")
			}
			if !isEnabledAdmin(cfg.UserID) {
				return SystemSettingsView{}, errors.New("回退 LLM 配置必须属于启用状态的管理员")
			}
		}
		if err := setting.SetLLMFallback(enabled, configID); err != nil {
			return SystemSettingsView{}, err
		}
	}

	// GitHub 凭证：任一项被提供则统一走 SetGitHubOAuth（secret 空串保留原值）。
	if in.GitHubClientID != nil || in.GitHubClientSecret != nil || in.GitHubOAuthEnabled != nil {
		clientID := setting.GitHubClientID()
		if in.GitHubClientID != nil {
			clientID = *in.GitHubClientID
		}
		secret := "" // 空表示保留原值
		if in.GitHubClientSecret != nil {
			secret = *in.GitHubClientSecret
		}
		enabled := setting.GitHubOAuthEnabled()
		if in.GitHubOAuthEnabled != nil {
			enabled = *in.GitHubOAuthEnabled
		}
		if enabled && clientID == "" {
			return SystemSettingsView{}, errors.New("启用 GitHub 登录前必须配置 client_id")
		}
		// 启用时必须已有可用 secret（本次提交的 或 已存库的），否则会出现“看似启用实则不可用”。
		if enabled && secret == "" && !setting.HasGitHubSecret() {
			return SystemSettingsView{}, errors.New("启用 GitHub 登录前必须配置 client_secret")
		}
		if err := setting.SetGitHubOAuth(clientID, secret, enabled); err != nil {
			return SystemSettingsView{}, err
		}
	}
	return s.GetSettings(), nil
}

// ---- 用户管理 ----

// ListUsers 列出全部用户（不含密码）。
func (s *AdminService) ListUsers() ([]model.User, error) {
	var users []model.User
	if err := common.DB.Order("id asc").Find(&users).Error; err != nil {
		return nil, err
	}
	for i := range users {
		users[i].Password = ""
	}
	return users, nil
}

// SetUserStatus 启用/禁用用户。禁用时吊销其全部刷新令牌（强制登出）。
func (s *AdminService) SetUserStatus(operatorID, targetID int64, status string) error {
	if status != model.StatusEnabled && status != model.StatusDisabled {
		return errors.New("非法的状态值")
	}
	if operatorID == targetID {
		return errors.New("不能修改自己的账号状态")
	}
	var target model.User
	if err := common.DB.First(&target, targetID).Error; err != nil {
		return errors.New("用户不存在")
	}
	if err := common.DB.Model(&target).Update("status", status).Error; err != nil {
		return err
	}
	if status == model.StatusDisabled {
		// 令牌版本 +1，令其旧 access token 即时失效；并吊销全部刷新令牌。
		common.DB.Model(&target).UpdateColumn("token_version", gorm.Expr("token_version + 1"))
		_ = NewAuthService().RevokeAllForUser(targetID)
	}
	return nil
}

// GetUserQuota 查看某用户的 AI 配额（无记录则按默认建一条）。
func (s *AdminService) GetUserQuota(userID int64) (*model.UserQuota, error) {
	var target model.User
	if err := common.DB.First(&target, userID).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

// QuotaUpdateInput 配额调整入参（次数制）。ActionLimit 0=不限；ResetUsed 清零已用次数与 token/请求数审计。
type QuotaUpdateInput struct {
	ActionLimit int64 `json:"action_limit"`
	ResetUsed   bool  `json:"reset_used"`
}

// UpdateUserQuota 调整某用户的次数上限，可选清零已用量（配额周期性手工重置的口子）。
func (s *AdminService) UpdateUserQuota(userID int64, in QuotaUpdateInput) (*model.UserQuota, error) {
	if in.ActionLimit < 0 {
		return nil, errors.New("action_limit 不能为负（0 表示不限）")
	}
	if _, err := s.GetUserQuota(userID); err != nil {
		return nil, err
	}
	updates := map[string]any{"action_limit": in.ActionLimit}
	if in.ResetUsed {
		updates["action_used"] = 0
		updates["token_used"] = 0
		updates["request_count"] = 0
	}
	if err := common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetUserQuota(userID)
}
