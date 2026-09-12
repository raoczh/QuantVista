package service

import (
	"errors"
	"sync"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLExperimentDetailUsesOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.LLMExperiment{}, &model.LLMExperimentRun{}, &model.LLMReleaseAudit{})
	experiment := model.LLMExperiment{UserID: 1971, Module: "recommendation", Status: model.ExpStatusRunning}
	if err := db.Create(&experiment).Error; err != nil {
		t.Fatal(err)
	}
	read, release := make(chan struct{}), make(chan struct{})
	var readOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_experiment_detail_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_experiments" {
			readOnce.Do(func() {
				close(read)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					tx.AddError(errors.New("reader was not released"))
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	type outcome struct {
		view *LLMExperimentDetailView
		err  error
	}
	done := make(chan outcome, 1)
	go func() { view, err := GetLLMExperimentDetail(experiment.ID); done <- outcome{view, err} }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("读取未进入实验查询")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model.LLMExperimentRun{ExperimentID: experiment.ID, RunStatus: model.LLMExperimentRunSuccess, Valid: true}).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.LLMReleaseAudit{ExperimentID: experiment.ID, Verdict: model.ReleaseAuditFail}).Error; err != nil {
			return err
		}
		return tx.Model(&experiment).Updates(map[string]any{"sample_count": 1, "status": model.ExpStatusCompleted}).Error
	}); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || result.view == nil {
			t.Fatalf("详情读取失败：%v", result.err)
		}
		view := result.view
		if view.Experiment.Status != model.ExpStatusRunning || view.Experiment.SampleCount != 0 || len(view.Runs) != 0 || len(view.Audits) != 0 {
			t.Fatalf("不能拼接旧实验状态与新样本/审计：%+v", view)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("详情读取未完成")
	}
}
