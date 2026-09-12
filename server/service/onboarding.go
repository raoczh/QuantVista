package service

import (
	"context"
	"errors"
	"strings"
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

var onboardingMutationGate = make(chan struct{}, 1)

func lockOnboardingMutation(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case onboardingMutationGate <- struct{}{}:
		return func() { <-onboardingMutationGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

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
	return readOnboardingProgressTx(tx, userID, version, create, false)
}

func lockedOnboardingProgressTx(tx *gorm.DB, userID int64, version int, create bool) (*model.OnboardingProgress, error) {
	return readOnboardingProgressTx(tx, userID, version, create, true)
}

func readOnboardingProgressTx(tx *gorm.DB, userID int64, version int, create, locking bool) (*model.OnboardingProgress, error) {
	if userID <= 0 || version <= 0 {
		return nil, errors.New("引导进度不存在")
	}
	query := func() *gorm.DB {
		q := tx.Where("user_id = ? AND version = ?", userID, version).Order("run DESC")
		if locking && !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		return q
	}
	var progress model.OnboardingProgress
	err := query().First(&progress).Error
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
	// 冲突时使用实际持久化进度；不要依赖不同驱动的 RowsAffected/LastInsertID 语义。
	progress = model.OnboardingProgress{}
	if err := query().First(&progress).Error; err != nil {
		return nil, err
	}
	return &progress, nil
}

func getOnboardingProgressVersion(userID int64, version int) (*OnboardingView, error) {
	return getOnboardingProgressVersionContext(context.Background(), userID, version)
}

func getOnboardingProgressVersionContext(ctx context.Context, userID int64, version int) (*OnboardingView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	progress, err := currentOnboardingProgressTx(common.DB.WithContext(ctx), userID, version, true)
	if err != nil {
		return nil, err
	}
	return onboardingView(*progress, time.Now()), nil
}

func GetOnboardingProgress(userID int64) (*OnboardingView, error) {
	return GetOnboardingProgressContext(context.Background(), userID)
}

func GetOnboardingProgressContext(ctx context.Context, userID int64) (*OnboardingView, error) {
	return getOnboardingProgressVersionContext(ctx, userID, OnboardingCurrentVersion)
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
	progress, err := lockedOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
	if err != nil {
		return err
	}
	return setOnboardingProgressStepTx(tx, progress, step, status, alertRuleID)
}

func setOnboardingProgressStepTx(tx *gorm.DB, progress *model.OnboardingProgress, step, status string, alertRuleID int64) error {
	step = strings.TrimSpace(step)
	statusColumn, timeColumn, err := onboardingStepColumns(step)
	if err != nil {
		return err
	}
	if status != model.OnboardingStepCompleted && status != model.OnboardingStepSkipped {
		return errors.New("非法的引导步骤状态")
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
	query := tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, progress.UserID)
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
	return SkipOnboardingStepContext(context.Background(), userID, step, 0)
}

func SkipOnboardingStepContext(ctx context.Context, userID int64, step string, progressID int64) (*OnboardingView, error) {
	return mutateOnboardingContext(ctx, userID, progressID, func(tx *gorm.DB, progress *model.OnboardingProgress) error {
		return setOnboardingProgressStepTx(tx, progress, step, model.OnboardingStepSkipped, 0)
	})
}

// 先锁当前轮次、核对页面看到的进度，再修改并在提交前构造完整返回值。
func mutateOnboardingContext(ctx context.Context, userID, progressID int64, mutate func(*gorm.DB, *model.OnboardingProgress) error) (*OnboardingView, error) {
	unlock, err := lockOnboardingMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var result model.OnboardingProgress
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		progress, err := lockedOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, progressID == 0)
		if err != nil {
			return err
		}
		if progressID != 0 && progress.ID != progressID {
			return errors.New("引导轮次已变化，请重新加载")
		}
		if err := mutate(tx, progress); err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ?", progress.ID, userID).First(&result).Error
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return onboardingView(result, time.Now()), nil
}

func RecordOnboardingAlertCreated(userID, ruleID int64) error {
	if ruleID <= 0 {
		return nil
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		return recordOnboardingAlertCreatedTx(tx, userID, ruleID)
	})
}

func recordOnboardingAlertCreatedTx(tx *gorm.DB, userID, ruleID int64) error {
	progress, err := lockedOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
	if err != nil || progress.Status == model.OnboardingStatusCompleted || stepTerminal(progress.AlertStatus) {
		return err
	}
	return tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID).
		Updates(map[string]any{"alert_rule_id": ruleID, "deferred_until": nil}).Error
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
		progress, err := lockedOnboardingProgressTx(tx, userID, OnboardingCurrentVersion, true)
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
		return setOnboardingProgressStepTx(tx, progress, OnboardingStepAlert, model.OnboardingStepCompleted, rule.ID)
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
	return FinishOnboardingContext(context.Background(), userID, 0)
}

func FinishOnboardingContext(ctx context.Context, userID, progressID int64) (*OnboardingView, error) {
	return mutateOnboardingContext(ctx, userID, progressID, func(tx *gorm.DB, progress *model.OnboardingProgress) error {
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
}

func RestartOnboarding(userID int64) (*OnboardingView, error) {
	return RestartOnboardingContext(context.Background(), userID, 0)
}

func RestartOnboardingContext(ctx context.Context, userID, progressID int64) (*OnboardingView, error) {
	if userID <= 0 {
		return nil, errors.New("引导进度不存在")
	}
	unlock, err := lockOnboardingMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var created model.OnboardingProgress
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest model.OnboardingProgress
		query := tx.Where("user_id = ? AND version = ?", userID, OnboardingCurrentVersion).Order("run DESC")
		if !common.UsingSQLite {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err := query.First(&latest).Error
		if progressID != 0 && (errors.Is(err, gorm.ErrRecordNotFound) || err == nil && latest.ID != progressID) {
			return errors.New("引导轮次已变化，请重新加载")
		}
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return onboardingView(created, time.Now()), nil
}

func DeferOnboarding(userID int64) (*OnboardingView, error) {
	return DeferOnboardingContext(context.Background(), userID, 0)
}

func DeferOnboardingContext(ctx context.Context, userID, progressID int64) (*OnboardingView, error) {
	return mutateOnboardingContext(ctx, userID, progressID, func(tx *gorm.DB, progress *model.OnboardingProgress) error {
		if progress.Status != model.OnboardingStatusCompleted {
			until := time.Now().Add(24 * time.Hour)
			return tx.Model(&model.OnboardingProgress{}).Where("id = ? AND user_id = ?", progress.ID, userID).
				Update("deferred_until", &until).Error
		}
		return nil
	})
}
