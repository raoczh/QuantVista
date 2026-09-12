package service

import (
	"errors"
	"fmt"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedJointReview(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	cleanJointTables(t)
	if err := common.DB.Create(&model.RecommendationBatch{ID: 2111, UserID: 1, Type: model.RecTypeShortTerm, Status: model.RecStatusSuccess}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		jointSeedLabel(t, int64(i), 2111, fmt.Sprintf("2026-06-%02d", i), model.RecActionBuy, 70, 2, -1, model.LabelMatured)
	}
}

func TestJointReviewAuditFailurePreservesEvidence(t *testing.T) {
	for _, failure := range []string{"read", "write", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			seedJointReview(t)
			original := `{"count":17,"last_at":"2026-06-30 10:00:00","log":["2026-06-30 10:00:00"]}`
			if failure == "corrupt" {
				original = `{"count":17`
			}
			if err := model.UpsertOption(jointLockedReadsKey, original); err != nil {
				t.Fatal(err)
			}
			fault := errors.New("本地注入锁定段审计故障")
			const callback = "review_joint_audit_failure"
			hook := func(tx *gorm.DB) {
				if tx.Statement.Table == "options" {
					tx.AddError(fault)
				}
			}
			remove := func() {
				_ = common.DB.Callback().Query().Remove(callback)
				_ = common.DB.Callback().Update().Remove(callback)
				_ = common.DB.Callback().Create().Remove(callback)
			}
			t.Cleanup(remove)
			if failure == "read" {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, hook); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "write" {
				if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, hook); err != nil {
					t.Fatal(err)
				}
				if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, hook); err != nil {
					t.Fatal(err)
				}
			}
			report, err := RunJointEval(true)
			if err == nil || report != nil {
				t.Errorf("审计不可持久核验时不能返回锁定段收益：err=%v report_returned=%v", err, report != nil)
			}
			if failure != "corrupt" && !errors.Is(err, fault) {
				t.Errorf("应保留原始审计故障：%v", err)
			}
			remove()
			var stored model.Option
			if err := common.DB.Where("`key` = ?", jointLockedReadsKey).First(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Value != original {
				t.Errorf("不能将不可读取的审计归零后覆盖：%s", stored.Value)
			}
			if failure != "corrupt" {
				report, err = RunJointEval(true)
				if err != nil || report == nil || report.LockedAudit == nil || report.LockedAudit.Count != 18 {
					t.Errorf("故障恢复后应从原先 17 次继续累计：err=%v report=%+v", err, report)
				}
			}
		})
	}
}

func TestJointReviewCachedAuditTracksLockedRead(t *testing.T) {
	seedJointReview(t)
	if _, err := RunJointEval(false); err != nil {
		t.Fatal(err)
	}
	if _, err := RunJointEval(true); err != nil {
		t.Fatal(err)
	}
	// 与管理接口相同：普通读取优先取缓存，缓存为空才重新计算。
	report := CachedJointEvalReport()
	if report == nil {
		var err error
		report, err = RunJointEval(false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if report.LockedAudit == nil || report.LockedAudit.Count != 1 {
		t.Fatalf("读取过锁定段后，普通页面不能仍显示从未读取：%+v", report.LockedAudit)
	}
}
