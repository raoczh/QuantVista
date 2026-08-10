package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func cleanOnboardingUser(t *testing.T, userID int64) {
	t.Helper()
	for _, target := range []any{
		&model.OnboardingProgress{}, &model.AlertRule{}, &model.WatchlistItem{},
		&model.Watchlist{}, &model.PositionTrade{}, &model.Position{}, &model.UserPreference{},
	} {
		if err := common.DB.Where("user_id = ?", userID).Delete(target).Error; err != nil {
			t.Fatalf("clean %T: %v", target, err)
		}
	}
}

func TestOnboardingProgressLifecycleAndIsolation(t *testing.T) {
	setupTestDB(t)
	userID, otherUserID := int64(89201), int64(89202)
	cleanOnboardingUser(t, userID)
	cleanOnboardingUser(t, otherUserID)

	first, err := GetOnboardingProgress(userID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != OnboardingCurrentVersion || first.Run != 1 || first.Status != model.OnboardingStatusInProgress ||
		first.PreferenceStatus != model.OnboardingStepNotStarted || first.SuggestedStep != OnboardingStepPreference || !first.ShouldPrompt {
		t.Fatalf("unexpected initial progress: %+v", first)
	}
	if _, err := FinishOnboarding(userID); err == nil {
		t.Fatal("unfinished onboarding was finished")
	}

	progress, err := SkipOnboardingStep(userID, OnboardingStepPreference)
	if err != nil || progress.PreferenceStatus != model.OnboardingStepSkipped || progress.SuggestedStep != OnboardingStepPortfolio {
		t.Fatalf("skip preference: %+v err=%v", progress, err)
	}
	if err := markOnboardingStepCompleted(userID, OnboardingStepPortfolio, 0); err != nil {
		t.Fatal(err)
	}
	// 没有本人提醒规则时，“立即检查”不能伪造第三步完成。
	if err := CompleteOnboardingAlertTest(userID); err != nil {
		t.Fatal(err)
	}
	progress, _ = GetOnboardingProgress(userID)
	if progress.AlertStatus != model.OnboardingStepNotStarted {
		t.Fatalf("alert completed without rule: %+v", progress)
	}
	rule := model.AlertRule{UserID: userID, Symbol: "600000", Market: "cn", Name: "测试", Kind: model.AlertKindPrice, Op: "gte", Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	// 账号已有规则并不等于本轮已创建提醒，未记录到本轮前不能完成第三步。
	if err := CompleteOnboardingAlertTest(userID); err != nil {
		t.Fatal(err)
	}
	progress, _ = GetOnboardingProgress(userID)
	if progress.AlertStatus != model.OnboardingStepNotStarted || progress.AlertRuleID != 0 {
		t.Fatalf("既有提醒不能冒充本轮创建并测试: %+v", progress)
	}
	if err := RecordOnboardingAlertCreated(userID, rule.ID); err != nil {
		t.Fatal(err)
	}
	progress, _ = GetOnboardingProgress(userID)
	if progress.AlertStatus != model.OnboardingStepNotStarted || progress.AlertRuleID != rule.ID || progress.AlertTestedAt != nil {
		t.Fatalf("create should not complete alert step: %+v", progress)
	}
	if err := common.DB.Model(&model.AlertRule{}).Where("id = ? AND user_id = ?", rule.ID, userID).Update("status", model.AlertStatusPaused).Error; err != nil {
		t.Fatal(err)
	}
	if err := CompleteOnboardingAlertTest(userID); err != nil {
		t.Fatal(err)
	}
	progress, _ = GetOnboardingProgress(userID)
	if progress.AlertStatus != model.OnboardingStepNotStarted {
		t.Fatalf("暂停的提醒不能冒充已经测试: %+v", progress)
	}
	if err := common.DB.Model(&model.AlertRule{}).Where("id = ? AND user_id = ?", rule.ID, userID).Update("status", model.AlertStatusActive).Error; err != nil {
		t.Fatal(err)
	}
	if err := CompleteOnboardingAlertTest(userID); err != nil {
		t.Fatal(err)
	}
	completed, err := FinishOnboarding(userID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != model.OnboardingStatusCompleted || completed.CompletedAt == nil || completed.AlertTestedAt == nil || completed.ShouldPrompt {
		t.Fatalf("unexpected completed progress: %+v", completed)
	}

	other, err := GetOnboardingProgress(otherUserID)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == completed.ID || other.PreferenceStatus != model.OnboardingStepNotStarted || other.AlertRuleID != 0 {
		t.Fatalf("cross-user progress leaked: %+v", other)
	}

	restarted, err := RestartOnboarding(userID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Run != 2 || restarted.Status != model.OnboardingStatusInProgress || restarted.AlertStatus != model.OnboardingStepNotStarted {
		t.Fatalf("unexpected restart: %+v", restarted)
	}
	var history int64
	common.DB.Model(&model.OnboardingProgress{}).Where("user_id = ? AND version = ?", userID, OnboardingCurrentVersion).Count(&history)
	if history != 2 {
		t.Fatalf("restart overwrote history, rows=%d", history)
	}
}

func TestOnboardingPreferenceHookDeferAndVersionUpgrade(t *testing.T) {
	setupTestDB(t)
	userID := int64(89203)
	cleanOnboardingUser(t, userID)

	input := PreferenceInput{
		RiskLevel: "balanced", DefaultMarket: "cn", HorizonPref: HorizonLongTerm,
		DefaultRecCount: 3, MinCandidateAmount: defaultMinCandidateAmount,
		TotalCapital: 100000, InvestmentGuideVersion: InvestmentGuideCurrentVersion,
		InvestmentGuideStatus: InvestmentGuideCompleted,
	}
	if _, err := NewUserService().UpdatePreference(userID, input); err != nil {
		t.Fatalf("UpdatePreference: %v", err)
	}
	progress, err := GetOnboardingProgress(userID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.PreferenceStatus != model.OnboardingStepCompleted || progress.PreferenceAt == nil {
		t.Fatalf("preference hook missing: %+v", progress)
	}
	deferred, err := DeferOnboarding(userID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred.ShouldPrompt || deferred.DeferredUntil == nil || !deferred.DeferredUntil.After(time.Now()) {
		t.Fatalf("defer did not suppress prompt: %+v", deferred)
	}
	restarted, err := RestartOnboarding(userID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Run != 2 || restarted.PreferenceStatus != model.OnboardingStepNotStarted || restarted.SuggestedStep != OnboardingStepPreference {
		t.Fatalf("explicit restart should begin a fresh run: %+v", restarted)
	}

	// 新流程版本必须有独立进度行；旧版本的 overall 完成不能直接冒充新版完成。
	watch := model.Watchlist{UserID: userID, Name: "已有自选"}
	if err := common.DB.Create(&watch).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.WatchlistItem{UserID: userID, WatchlistID: watch.ID, Symbol: "000001", Market: "cn", Name: "平安银行"}).Error; err != nil {
		t.Fatal(err)
	}
	upgraded, err := getOnboardingProgressVersion(userID, OnboardingCurrentVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != 2 || upgraded.Run != 1 || upgraded.Status != model.OnboardingStatusInProgress {
		t.Fatalf("version upgrade reused old overall state: %+v", upgraded)
	}
	if upgraded.PreferenceStatus != model.OnboardingStepCompleted || upgraded.PortfolioStatus != model.OnboardingStepCompleted || upgraded.AlertStatus != model.OnboardingStepNotStarted {
		t.Fatalf("version upgrade did not restore explicit facts: %+v", upgraded)
	}
}

func TestOnboardingSkipIsExplicitAndIdempotent(t *testing.T) {
	setupTestDB(t)
	userID := int64(89204)
	cleanOnboardingUser(t, userID)
	for _, step := range []string{OnboardingStepPreference, OnboardingStepPortfolio, OnboardingStepAlert} {
		first, err := SkipOnboardingStep(userID, step)
		if err != nil {
			t.Fatalf("skip %s: %v", step, err)
		}
		second, err := SkipOnboardingStep(userID, step)
		if err != nil || first.ID != second.ID || first.Run != second.Run {
			t.Fatalf("repeat skip %s did not converge: first=%+v second=%+v err=%v", step, first, second, err)
		}
	}
	finished, err := FinishOnboarding(userID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.PreferenceStatus != model.OnboardingStepSkipped || finished.PortfolioStatus != model.OnboardingStepSkipped ||
		finished.AlertStatus != model.OnboardingStepSkipped || finished.Status != model.OnboardingStatusCompleted {
		t.Fatalf("skip facts were not preserved: %+v", finished)
	}
}

func TestOnboardingModelsAutoMigrateIdempotently(t *testing.T) {
	setupTestDB(t)
	if !common.DB.Migrator().HasTable(&model.OnboardingProgress{}) ||
		!common.DB.Migrator().HasTable(&model.WatchlistBatch{}) ||
		!common.DB.Migrator().HasTable(&model.WatchlistBatchItem{}) {
		t.Fatal("onboarding/watchlist batch models are missing from AllModels")
	}
	if err := common.DB.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatalf("repeated AutoMigrate: %v", err)
	}
}
