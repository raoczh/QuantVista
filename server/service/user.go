package service

import (
	"errors"

	"quantvista/common"
	"quantvista/model"
)

type UserService struct{}

func NewUserService() *UserService { return &UserService{} }

// GetByID 取用户（不含密码）。
func (s *UserService) GetByID(id int64) (*model.User, error) {
	var u model.User
	if err := common.DB.First(&u, id).Error; err != nil {
		return nil, errors.New("用户不存在")
	}
	u.Password = ""
	return &u, nil
}

// GetPreference 取用户偏好，不存在则建默认。
func (s *UserService) GetPreference(userID int64) (*model.UserPreference, error) {
	var p model.UserPreference
	if err := common.DB.FirstOrCreate(&p, model.UserPreference{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// PreferenceInput 偏好更新入参。
type PreferenceInput struct {
	RiskLevel       string `json:"risk_level"`
	DefaultMarket   string `json:"default_market"`
	HorizonPref     string `json:"horizon_pref"`
	DefaultRecCount int    `json:"default_rec_count"`
	EnableNotify    bool   `json:"enable_notify"`
}

var (
	validRisk    = map[string]bool{"conservative": true, "balanced": true, "aggressive": true}
	validMarket  = map[string]bool{"cn": true, "us": true, "hk": true}
	validHorizon = map[string]bool{"short_term": true, "long_term": true}
)

// UpdatePreference 校验并更新用户偏好。
func (s *UserService) UpdatePreference(userID int64, in PreferenceInput) (*model.UserPreference, error) {
	if !validRisk[in.RiskLevel] {
		return nil, errors.New("非法的风险等级")
	}
	if !validMarket[in.DefaultMarket] {
		return nil, errors.New("非法的默认市场")
	}
	if !validHorizon[in.HorizonPref] {
		return nil, errors.New("非法的默认周期")
	}
	if in.DefaultRecCount < 1 || in.DefaultRecCount > 20 {
		return nil, errors.New("默认推荐数量需在 1~20 之间")
	}
	p, err := s.GetPreference(userID)
	if err != nil {
		return nil, err
	}
	p.RiskLevel = in.RiskLevel
	p.DefaultMarket = in.DefaultMarket
	p.HorizonPref = in.HorizonPref
	p.DefaultRecCount = in.DefaultRecCount
	p.EnableNotify = in.EnableNotify
	if err := common.DB.Save(p).Error; err != nil {
		return nil, err
	}
	return p, nil
}

// GetQuota 取用户配额，不存在则建默认。
func (s *UserService) GetQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}
