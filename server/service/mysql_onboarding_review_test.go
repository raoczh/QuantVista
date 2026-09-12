package service

import (
	"sync"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLOnboardingWaitsForCurrentProgress(t *testing.T) {
	for _, action := range []string{"finish_after_restart", "test_after_new_rule"} {
		t.Run(action, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.OnboardingProgress{}, &model.AlertRule{})
			progress := newOnboardingProgress(89303, OnboardingCurrentVersion, 1)
			var rules []model.AlertRule
			if action == "finish_after_restart" {
				progress.PreferenceStatus, progress.PortfolioStatus, progress.AlertStatus = model.OnboardingStepSkipped, model.OnboardingStepSkipped, model.OnboardingStepSkipped
			} else {
				rules = []model.AlertRule{
					{UserID: progress.UserID, Symbol: "600000", Market: "cn", Kind: model.AlertKindPrice, Op: "gte", Threshold: 10, Status: model.AlertStatusActive},
					{UserID: progress.UserID, Symbol: "000001", Market: "cn", Kind: model.AlertKindPrice, Op: "gte", Threshold: 10, Status: model.AlertStatusActive},
				}
				if err := db.Create(&rules).Error; err != nil {
					t.Fatal(err)
				}
				progress.AlertRuleID = rules[0].ID
			}
			if err := db.Create(&progress).Error; err != nil {
				t.Fatal(err)
			}
			writer := db.Begin()
			if writer.Error != nil {
				t.Fatal(writer.Error)
			}
			t.Cleanup(func() { writer.Rollback() })
			var locked model.OnboardingProgress
			if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, progress.ID).Error; err != nil {
				t.Fatal(err)
			}
			if action == "finish_after_restart" {
				next := newOnboardingProgress(progress.UserID, OnboardingCurrentVersion, 2)
				if err := writer.Create(&next).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := writer.Model(&progress).Update("alert_rule_id", rules[1].ID).Error; err != nil {
				t.Fatal(err)
			}
			started, read := make(chan struct{}), make(chan struct{})
			var beforeOnce, afterOnce sync.Once
			const beforeCallback = "review_onboarding_current_progress_before"
			const afterCallback = "review_onboarding_current_progress_after"
			if err := db.Callback().Query().Before("gorm:query").Register(beforeCallback, func(tx *gorm.DB) {
				if tx.Statement.Table == "onboarding_progresses" {
					beforeOnce.Do(func() { close(started) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Query().After("gorm:query").Register(afterCallback, func(tx *gorm.DB) {
				if tx.Statement.Table == "onboarding_progresses" {
					afterOnce.Do(func() { close(read) })
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Query().Remove(beforeCallback); db.Callback().Query().Remove(afterCallback) })
			done := make(chan error, 1)
			go func() {
				if action == "finish_after_restart" {
					_, err := FinishOnboarding(progress.UserID)
					done <- err
				} else {
					done <- completeOnboardingAlertTest(progress.UserID, rules[0].ID)
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("引导处理没有进入查询")
			}
			// 普通一致性读会立即拿到旧行；正确的锁定读在 writer 提交前保持等待。
			select {
			case <-read:
			case <-time.After(500 * time.Millisecond):
			}
			if err := writer.Commit().Error; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if action == "finish_after_restart" && err == nil {
					t.Error("新一轮尚未开始，仍使用上一轮已跳过状态报告完成成功")
				}
				if action == "test_after_new_rule" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("释放事务后引导处理没有结束")
			}
			var stored model.OnboardingProgress
			if err := db.First(&stored, progress.ID).Error; err != nil {
				t.Fatal(err)
			}
			if action == "finish_after_restart" && stored.Status == model.OnboardingStatusCompleted {
				t.Error("新轮次创建后仍结束了旧轮次")
			}
			if action == "test_after_new_rule" && (stored.AlertStatus != model.OnboardingStepNotStarted || stored.AlertRuleID != rules[1].ID || stored.AlertTestedAt != nil) {
				t.Errorf("旧提醒测试覆盖新规则并冒充已测试：%+v", stored)
			}
		})
	}
}
