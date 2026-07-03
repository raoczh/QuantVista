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
	RegistrationOpen   bool   `json:"registration_open"`
	GitHubOAuthEnabled bool   `json:"github_oauth_enabled"`
	GitHubClientID     string `json:"github_client_id"`
	HasGitHubSecret    bool   `json:"has_github_secret"`
}

// GetSettings 读取当前系统设置。
func (s *AdminService) GetSettings() SystemSettingsView {
	return SystemSettingsView{
		RegistrationOpen:   setting.RegistrationOpen(),
		GitHubOAuthEnabled: setting.GitHubOAuthEnabled(),
		GitHubClientID:     setting.GitHubClientID(),
		HasGitHubSecret:    setting.HasGitHubSecret(),
	}
}

// UpdateSettingsInput 部分更新；指针为 nil 表示该项不变。GitHubClientSecret 为空字符串表示保留原值。
type UpdateSettingsInput struct {
	RegistrationOpen   *bool   `json:"registration_open"`
	GitHubOAuthEnabled *bool   `json:"github_oauth_enabled"`
	GitHubClientID     *string `json:"github_client_id"`
	GitHubClientSecret *string `json:"github_client_secret"`
}

// UpdateSettings 应用系统设置变更。
func (s *AdminService) UpdateSettings(in UpdateSettingsInput) (SystemSettingsView, error) {
	if in.RegistrationOpen != nil {
		if err := setting.SetRegistrationOpen(*in.RegistrationOpen); err != nil {
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

// QuotaUpdateInput 配额调整入参。TokenLimit 0=不限；ResetUsed 清零已用量与请求数。
type QuotaUpdateInput struct {
	TokenLimit int64 `json:"token_limit"`
	ResetUsed  bool  `json:"reset_used"`
}

// UpdateUserQuota 调整某用户的 token 上限，可选清零已用量（配额周期性手工重置的口子）。
func (s *AdminService) UpdateUserQuota(userID int64, in QuotaUpdateInput) (*model.UserQuota, error) {
	if in.TokenLimit < 0 {
		return nil, errors.New("token_limit 不能为负（0 表示不限）")
	}
	if _, err := s.GetUserQuota(userID); err != nil {
		return nil, err
	}
	updates := map[string]any{"token_limit": in.TokenLimit}
	if in.ResetUsed {
		updates["token_used"] = 0
		updates["request_count"] = 0
	}
	if err := common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetUserQuota(userID)
}
