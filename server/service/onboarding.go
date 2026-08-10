package service

import (
	"errors"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const OnboardingCurrentVersion = 1

const (
	OnboardingStepPreference = "preference"
	OnboardingStepPortfolio  = "portfolio"
	OnboardingStepAlert      = "alert"
)

var onboardingMutationMu sync.Mutex

type OnboardingView struct {
	model.OnboardingProgress
	ShouldPrompt  bool   `json:"should_prompt"`
	SuggestedStep string `json:"suggested_step"`
}

func stepTerminal(status string) bool {
	return status == model.OnboardingStepCompleted || status == model.OnboardingStepSkipped
}

func onboardingView(progress model.OnboardingProgress, now time.Time) *OnboardingView {
	step := "complete"
	switch {
	case !stepTerminal(progress.PreferenceStatus):
		step = OnboardingStepPreference
	case !stepTerminal(progress.PortfolioStatus):
		step = OnboardingStepPortfolio
	case !stepTerminal(progress.AlertStatus):
		step = OnboardingStepAlert
	}
	shouldPrompt := progress.Status != model.OnboardingStatusCompleted &&
		(progress.DeferredUntil == nil || !progress.DeferredUntil.After(now))
	return &OnboardingView{OnboardingProgress: progress, ShouldPrompt: shouldPrompt, SuggestedStep: step}
}

func newOnboardingProgress(userID int64, version, run int) model.OnboardingProgress {
	return model.OnboardingProgress{
		UserID: userID, Version: version, Run: run, Status: model.OnboardingStatusInProgress,
		PreferenceStatus: model.OnboardingStepNotStarted,
		PortfolioStatus:  model.OnboardingStepNotStarted,
		AlertStatus:      model.OnboardingStepNotStarted,
	}
}

func seededOnboardingProgress(tx *gorm.DB, userID int64, version, run int) (model.OnboardingProgress, error) {
	now := time.Now()
	progress := newOnboardingProgress(userID, version, run)
	// 兼容功能上线前已经明确完成/跳过三问的用户；数据库默认值不参与判断。
	var preference model.UserPreference
	err := tx.Where("user_id = ?", userID).First(&preference).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return progress, err
	}
	if err == nil && preference.InvestmentGuideVersion > 0 {
		switch preference.InvestmentGuideStatus {
		case InvestmentGuideCompleted:
			progress.PreferenceStatus, progress.PreferenceAt = model.OnboardingStepCompleted, &now
		case InvestmentGuideSkipped:
			progress.PreferenceStatus, progress.PreferenceAt = model.OnboardingStepSkipped, &now
		}
	}
	// 只依据真实业务行恢复第二步，不从偏好或默认字段猜测。
	var watchCount, positionCount int64
	if err := tx.Model(&model.WatchlistItem{}).Where("user_id = ?", userID).Count(&watchCount).Error; err != nil {
		return progress, err
	}
	if err := tx.Model(&model.Position{}).Where("user_id = ?", userID).Count(&positionCount).Error; err != nil {
		return progress, err
	}
	if watchCount > 0 || positionCount > 0 {
		progress.PortfolioStatus, progress.PortfolioAt = model.OnboardingStepCompleted, &now
	}
	return progress, nil
}

func currentOnboardingProgressTx(tx *gorm.DB, userID int64, version int, create bool) (*model.OnboardingProgress, error) {
	if userID <= 0 || version <= 0 {
		return nil, errors.New("引导进度不存在")
	}
	var progress model.OnboardingProgress
	err := tx.Where("user_id = ? AND version = ?", userID, version).Order("run DESC").First(&progress).Error
	if err == nil {
		return &progress, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if !create {
		return nil, errors.New("引导进度不存在")
	}
	progress, err = seededOnboardingProgress(tx, userID, version, 1)
	if err != nil {
		return nil, err
	}
	created := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "version"}, {Name: "run"}}, DoNothing: true,
	}).Create(&progress)
	if created.Error != nil {
		return nil, created.Error
	}
	if created.RowsAffected == 0 {
		if err := tx.Where("user_id = ? AND version = ?", userID, version).Order("run DESC").First(&progress).Error; err != nil {
			return nil, err
		}
	}
	return &progress, nil
}

func getOnboardingProgressVersion(userID int64, version int) (*OnboardingView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	progress, err := currentOnboardingProgressTx(common.DB, userID, version, true)
	if err != nil {
		return nil, err
	}
	return onboardingView(*progress, time.Now()), nil
}

func GetOnboardingProgress(userID int64) (*OnboardingView, error) {
	return getOnboardingProgressVersion(userID, OnboardingCurrentVersion)
}

func onboardingStepColumns(step string) (string, string, error) {
	switch strings.TrimSpace(step) {
	case OnboardingStepPreference:
		return "preference_status", "preference_at", nil
	case OnboardingStepPortfolio:
		return "portfolio_status", "portfolio_at", nil
	case OnboardingStepAlert:
		return "alert_status", "alert_at", nil
	default:
		return "", "", errors.New("未知的引导步骤")
	}
}

func setOnboardingStepTx(tx *gorm.DB, userID int64, step, status string, alertRuleID int64) error {
	statusColumn, timeColumn, err := onboardingStepColumns(step)
	if err != nil {
		return err
	}
	if status != model.OnboardingStepCompleted && status != model.OnboardingStepSkipped {
		return errors.New("非法的引导步骤状态")
	}
	progress, err := currentOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
	if err != nil {
		return err
	}
	if progress.Status == model.OnboardingStatusCompleted {
		return nil
	}
	currentStatus := map[string]string{
		OnboardingStepPreference: progress.PreferenceStatus,
		OnboardingStepPortfolio:  progress.PortfolioStatus,
		OnboardingStepAlert:      progress.AlertStatus,
	}[step]
	if currentStatus == model.OnboardingStepCompleted && status == model.OnboardingStepSkipped {
		return nil
	}
	if currentStatus == status {
		return nil
	}
	now := time.Now()
	updates := map[string]any{statusColumn: status, timeColumn: &now, "deferred_until": nil}
	if step == OnboardingStepAlert && alertRuleID > 0 {
		updates["alert_rule_id"] = alertRuleID
		if status == model.OnboardingStepCompleted {
			updates["alert_tested_at"] = &now
		}
	}
	query := tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID)
	if status == model.OnboardingStepSkipped {
		// 业务动作的 completed 在并发下优先，跳过不能覆盖已经完成的事实。
		query = query.Where(statusColumn+" <> ?", model.OnboardingStepCompleted)
	}
	return query.Updates(updates).Error
}

func markOnboardingStepCompleted(userID int64, step string, alertRuleID int64) error {
	return common.DB.Transaction(func(tx *gorm.DB) error {
		return setOnboardingStepTx(tx, userID, step, model.OnboardingStepCompleted, alertRuleID)
	})
}

func SkipOnboardingStep(userID int64, step string) (*OnboardingView, error) {
	onboardingMutationMu.Lock()
	defer onboardingMutationMu.Unlock()
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		return setOnboardingStepTx(tx, userID, step, model.OnboardingStepSkipped, 0)
	}); err != nil {
		return nil, err
	}
	return GetOnboardingProgress(userID)
}

func RecordOnboardingAlertCreated(userID, ruleID int64) error {
	if ruleID <= 0 {
		return nil
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		progress, err := currentOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
		if err != nil || progress.Status == model.OnboardingStatusCompleted || progress.AlertStatus == model.OnboardingStepSkipped {
			return err
		}
		return tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID).
			Updates(map[string]any{"alert_rule_id": ruleID, "deferred_until": nil}).Error
	})
}

func activeOnboardingAlertRule(userID int64) (int64, error) {
	progress, err := currentOnboardingProgressTx(common.DB, userID, OnboardingCurrentVersion, true)
	if err != nil {
		return 0, err
	}
	if progress.AlertStatus == model.OnboardingStepCompleted || progress.AlertStatus == model.OnboardingStepSkipped || progress.AlertRuleID <= 0 {
		return 0, nil
	}
	var count int64
	if err := common.DB.Model(&model.AlertRule{}).Where("user_id = ? AND id = ? AND status = ?", userID, progress.AlertRuleID, model.AlertStatusActive).Count(&count).Error; err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, nil
	}
	return progress.AlertRuleID, nil
}

func completeOnboardingAlertTest(userID, testedRuleID int64) error {
	if testedRuleID <= 0 {
		return nil
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		progress, err := currentOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
		if err != nil {
			return err
		}
		if progress.AlertStatus == model.OnboardingStepCompleted || progress.AlertStatus == model.OnboardingStepSkipped || progress.AlertRuleID != testedRuleID {
			return nil
		}
		var rule model.AlertRule
		query := tx.Where("user_id = ? AND id = ?", userID, testedRuleID)
		if err := query.First(&rule).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		return setOnboardingStepTx(tx, userID, OnboardingStepAlert, model.OnboardingStepCompleted, rule.ID)
	})
}

func CompleteOnboardingAlertTest(userID int64) error {
	ruleID, err := activeOnboardingAlertRule(userID)
	if err != nil {
		return err
	}
	return completeOnboardingAlertTest(userID, ruleID)
}

func FinishOnboarding(userID int64) (*OnboardingView, error) {
	onboardingMutationMu.Lock()
	defer onboardingMutationMu.Unlock()
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		progress, err := currentOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
		if err != nil {
			return err
		}
		if progress.Status == model.OnboardingStatusCompleted {
			return nil
		}
		if !stepTerminal(progress.PreferenceStatus) || !stepTerminal(progress.PortfolioStatus) || !stepTerminal(progress.AlertStatus) {
			return errors.New("请先完成或跳过前面的引导步骤")
		}
		now := time.Now()
		return tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID).Updates(map[string]any{
			"status": model.OnboardingStatusCompleted, "completed_at": &now, "deferred_until": nil,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return GetOnboardingProgress(userID)
}

func RestartOnboarding(userID int64) (*OnboardingView, error) {
	onboardingMutationMu.Lock()
	defer onboardingMutationMu.Unlock()
	var created model.OnboardingProgress
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var latest model.OnboardingProgress
		query := tx.Where("user_id = ? AND version = ?", userID, OnboardingCurrentVersion).Order("run DESC")
		if !common.UsingSQLite {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err := query.First(&latest).Error
		nextRun := 1
		if err == nil {
			nextRun = latest.Run + 1
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// 主动重跑从明确的未开始状态起步；用户仍可重新使用同一偏好、导入和提醒表单。
		// 只有功能升级后首次建立进度时才根据历史显式事实恢复，避免重跑被旧事实直接跳过。
		created = newOnboardingProgress(userID, OnboardingCurrentVersion, nextRun)
		return tx.Create(&created).Error
	})
	if err != nil {
		return nil, err
	}
	return onboardingView(created, time.Now()), nil
}

func DeferOnboarding(userID int64) (*OnboardingView, error) {
	onboardingMutationMu.Lock()
	defer onboardingMutationMu.Unlock()
	progress, err := currentOnboardingProgressTx(common.DB, userID, OnboardingCurrentVersion, true)
	if err != nil {
		return nil, err
	}
	if progress.Status != model.OnboardingStatusCompleted {
		until := time.Now().Add(24 * time.Hour)
		if err := common.DB.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID).
			Update("deferred_until", &until).Error; err != nil {
			return nil, err
		}
	}
	return GetOnboardingProgress(userID)
}
