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
	LLMCapabilityRouting   bool   `json:"llm_capability_routing"`
	LLMConditionalDebate   bool   `json:"llm_conditional_debate"`
	LLMReflectionShadow    bool   `json:"llm_reflection_shadow"`
	LLMChallenger          bool   `json:"llm_challenger"`
	LLMLayeredContext      bool   `json:"llm_layered_context"`
	LLMModelRouting        bool   `json:"llm_model_routing"`
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
		LLMCapabilityRouting:   setting.LLMCapabilityRouting(),
		LLMConditionalDebate:   setting.LLMConditionalDebate(),
		LLMReflectionShadow:    setting.LLMReflectionShadow(),
		LLMChallenger:          setting.LLMChallenger(),
		LLMLayeredContext:      setting.LLMLayeredContext(),
		LLMModelRouting:        setting.LLMModelRouting(),
		SiteBaseURL:            setting.SiteBaseURL(),
	}
}

// UpdateSettingsInput 与 setting 共用部分更新契约，确保校验、持久化和内存发布原子完成。
type UpdateSettingsInput = setting.UpdateInput

func (s *AdminService) UpdateSettings(in UpdateSettingsInput) (SystemSettingsView, error) {
	if in.LLMFallbackEnabled != nil || in.LLMFallbackConfigID != nil {
		configID := setting.LLMFallbackConfigID()
		if in.LLMFallbackConfigID != nil {
			configID = *in.LLMFallbackConfigID
		}
		if configID > 0 {
			var cfg model.LLMConfig
			if err := common.DB.Select("id, user_id").First(&cfg, configID).Error; err != nil {
				return SystemSettingsView{}, errors.New("指定的回退 LLM 配置不存在")
			}
			if !isEnabledAdmin(cfg.UserID) {
				return SystemSettingsView{}, errors.New("回退 LLM 配置必须属于启用状态的管理员")
			}
		}
	}
	if err := setting.Update(in); err != nil {
		return SystemSettingsView{}, err
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
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var target model.User
		if err := tx.First(&target, targetID).Error; err != nil {
			return errors.New("用户不存在")
		}
		updates := map[string]any{"status": status}
		if status == model.StatusDisabled {
			updates["token_version"] = gorm.Expr("token_version + 1")
		}
		if err := tx.Model(&target).Updates(updates).Error; err != nil {
			return err
		}
		if status == model.StatusDisabled {
			return tx.Model(&model.RefreshToken{}).
				Where("user_id = ? AND revoked = ?", targetID, false).Update("revoked", true).Error
		}
		return nil
	})
	if err == nil {
		invalidateLLMRouteCache()
	}
	return err
}

// GetUserQuota 查看某用户的 AI 配额（无记录则按默认建一条）。
func (s *AdminService) GetUserQuota(userID int64) (*model.UserQuota, error) {
	var target model.User
	if err := common.DB.First(&target, userID).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	return getUserQuota(userID)
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
		updates["action_epoch"] = gorm.Expr("action_epoch + 1")
		updates["action_used"] = 0
		updates["token_used"] = 0
		updates["request_count"] = 0
	}
	if err := common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetUserQuota(userID)
}
