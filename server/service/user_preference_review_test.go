package service

import (
	"encoding/json"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func preferenceReviewInput(t *testing.T, raw string) PreferenceInput {
	t.Helper()
	var in PreferenceInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func preferenceReviewValue[T any](value T) *T { return &value }

func TestPreferencePartialUpdatePreservesOtherPages(t *testing.T) {
	setupTestDB(t)
	pref := model.UserPreference{UserID: 1040, TotalCapital: 200000, EnableNotify: true,
		InvestmentGuideVersion: 1, InvestmentGuideStatus: InvestmentGuideCompleted,
		GuardConfigJSON: `{"pos_pct_threshold":13.5}`, RecFiltersJSON: `{"price_max":20}`}
	if err := common.DB.Create(&pref).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewUserService()
	updated, err := svc.UpdatePreference(pref.UserID, preferenceReviewInput(t, `{"rec_filters_json":"{\"price_max\":30}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.EnableNotify || updated.TotalCapital != 200000 || updated.GuardConfigJSON != pref.GuardConfigJSON ||
		updated.InvestmentGuideStatus != InvestmentGuideCompleted {
		t.Fatalf("保存筛选不得覆盖其他页面的偏好：%+v", updated)
	}
	filters := updated.RecFiltersJSON
	updated, err = svc.UpdatePreference(pref.UserID, preferenceReviewInput(t, `{"enable_notify":false,"min_candidate_amount":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if updated.EnableNotify || updated.MinCandidateAmount != 0 || updated.TotalCapital != 200000 || updated.RecFiltersJSON != filters {
		t.Fatalf("显式 false/0 应保存且不覆盖筛选与资金：%+v", updated)
	}
	if _, err := svc.UpdatePreference(pref.UserID, preferenceReviewInput(t, `{"total_capital":0}`)); err == nil {
		t.Fatal("部分更新仍须校验合并后的向导与资金状态")
	}
	stored, err := svc.GetPreference(pref.UserID)
	if err != nil || stored.TotalCapital != 200000 {
		t.Fatalf("无效更新必须整体回滚：pref=%+v err=%v", stored, err)
	}
}

func TestPreferenceStaleGuideSkipCannotUndoCompletion(t *testing.T) {
	setupTestDB(t)
	svc := NewUserService()
	if _, err := svc.UpdatePreference(1041, preferenceReviewInput(t,
		`{"investment_guide_version":1,"investment_guide_status":"completed","total_capital":100000}`)); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdatePreference(1041, preferenceReviewInput(t,
		`{"investment_guide_version":1,"investment_guide_status":"skipped"}`))
	if err != nil || updated.InvestmentGuideStatus != InvestmentGuideCompleted || updated.TotalCapital != 100000 {
		t.Fatalf("旧页面的跳过操作不得撤销已完成向导：pref=%+v err=%v", updated, err)
	}
}
