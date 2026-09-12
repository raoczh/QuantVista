package service

import (
	"errors"
	"reflect"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestOnboardingReplyReadFailureCannotLeaveCommittedChange(t *testing.T) {
	for _, action := range []string{"skip", "finish", "defer"} {
		t.Run(action, func(t *testing.T) {
			setupTestDB(t)
			const userID int64 = 89301
			cleanOnboardingUser(t, userID)
			if _, err := GetOnboardingProgress(userID); err != nil {
				t.Fatal(err)
			}
			if action == "finish" {
				for _, step := range []string{OnboardingStepPreference, OnboardingStepPortfolio, OnboardingStepAlert} {
					if _, err := SkipOnboardingStep(userID, step); err != nil {
						t.Fatal(err)
					}
				}
			}
			var before model.OnboardingProgress
			if err := common.DB.Where("user_id = ?", userID).First(&before).Error; err != nil {
				t.Fatal(err)
			}
			written := false
			const callback = "review_onboarding_reply_read_failure"
			if err := common.DB.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "onboarding_progresses" {
					written = true
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if written && tx.Statement.Table == "onboarding_progresses" {
					tx.AddError(errors.New("本机模拟返回进度读取失败"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			cleanup := func() { common.DB.Callback().Update().Remove(callback); common.DB.Callback().Query().Remove(callback) }
			t.Cleanup(cleanup)
			var err error
			switch action {
			case "skip":
				_, err = SkipOnboardingStep(userID, OnboardingStepPreference)
			case "finish":
				_, err = FinishOnboarding(userID)
			case "defer":
				_, err = DeferOnboarding(userID)
			}
			cleanup()
			var after model.OnboardingProgress
			if readErr := common.DB.First(&after, before.ID).Error; readErr != nil {
				t.Fatal(readErr)
			}
			if !written || err == nil {
				t.Fatalf("没有复现写后读取失败：written=%v err=%v", written, err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("接口报错却留下已提交引导变更：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOnboardingCompletedAlertRetainsTestedRule(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 89302
	cleanOnboardingUser(t, userID)
	rules := []model.AlertRule{
		{UserID: userID, Symbol: "600000", Market: "cn", Kind: model.AlertKindPrice, Op: "gte", Threshold: 10, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "000001", Market: "cn", Kind: model.AlertKindPrice, Op: "gte", Threshold: 10, Status: model.AlertStatusActive},
	}
	for i := range rules {
		if err := common.DB.Create(&rules[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordOnboardingAlertCreated(userID, rules[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := CompleteOnboardingAlertTest(userID); err != nil {
		t.Fatal(err)
	}
	before, err := GetOnboardingProgress(userID)
	if err != nil {
		t.Fatal(err)
	}
	if before.AlertStatus != model.OnboardingStepCompleted || before.AlertTestedAt == nil {
		t.Fatalf("测试前置状态未完成：%+v", before)
	}
	if err := RecordOnboardingAlertCreated(userID, rules[1].ID); err != nil {
		t.Fatal(err)
	}
	after, err := GetOnboardingProgress(userID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AlertRuleID != before.AlertRuleID || !after.AlertTestedAt.Equal(*before.AlertTestedAt) {
		t.Errorf("已通过检查的提醒被换成未检查规则：before=%+v after=%+v", before, after)
	}
}
