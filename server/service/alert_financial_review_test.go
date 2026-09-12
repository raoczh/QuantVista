package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestAlertFinancialReviewRescheduledDisclosureIsNewFact(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940013
	now := time.Now()
	rule := model.AlertRule{UserID: userID, Symbol: "600013", Market: "cn", Kind: model.AlertKindEarnDate, Op: model.AlertOpGTE, Threshold: 3, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	schedule := model.DisclosureSchedule{Symbol: rule.Symbol, Market: "cn", ReportDate: "2026-06-30", AppointDate: now.AddDate(0, 0, 2).Format("2006-01-02"), ReportTypeName: "中报"}
	if err := common.DB.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	svc := &AlertService{}
	var lastHit time.Time
	for i, want := range []int{1, 0, 1, 0} {
		if i == 2 {
			if err := common.DB.Model(&schedule).Update("appoint_date", now.AddDate(0, 0, 1).Format("2006-01-02")).Error; err != nil {
				t.Fatal(err)
			}
		}
		if hits, err := svc.evaluateEarnRulesForUserContext(t.Context(), userID); err != nil || hits != want {
			t.Errorf("第 %d 次评估：预约日提前应产生新事实，其余重复不应新增；hits=%d want=%d err=%v", i+1, hits, want, err)
		}
		var stored model.AlertRule
		if err := common.DB.First(&stored, rule.ID).Error; err != nil || stored.TriggeredAt == nil {
			t.Fatalf("读取命中时间失败：rule=%+v err=%v", stored, err)
		}
		if want == 0 && !stored.TriggeredAt.Equal(lastHit) {
			t.Error("重复财报事实不应推进最近命中时间")
		}
		lastHit = *stored.TriggeredAt
	}
	var count int64
	if err := common.DB.Model(&model.AlertEvent{}).Where("rule_id = ?", rule.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("两次不同预约事实应各留一条事件，实际 %d 条", count)
	}
}

func TestMySQLAlertFinancialWaitsForPublishedSource(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.AlertRule{}, &model.AlertEvent{}, &model.DisclosureSchedule{})
	now := time.Now()
	rule := model.AlertRule{UserID: 940016, Symbol: "600016", Market: "cn", Kind: model.AlertKindEarnDate, Op: model.AlertOpGTE, Threshold: 3, Status: model.AlertStatusActive}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	schedule := model.DisclosureSchedule{Symbol: rule.Symbol, Market: "cn", ReportDate: "2026-06-30", AppointDate: now.AddDate(0, 0, 1).Format("2006-01-02")}
	if err := db.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	writer := db.Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	t.Cleanup(func() { writer.Rollback() })
	if err := writer.Model(&schedule).Update("is_published", true).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	const hook = "review_mysql_alert_financial_lock"
	if err := db.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, locking := tx.Statement.Clauses["FOR"]; tx.Statement.Table == "disclosure_schedules" && locking {
			once.Do(func() { close(waiting) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(hook) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		hits, err := (&AlertService{}).evaluateEarnRulesForUserContext(ctx, rule.UserID)
		if err == nil && hits != 0 {
			err = fmt.Errorf("等待发布事务后仍提交 %d 条过时提醒", hits)
		}
		result <- err
	}()
	select {
	case <-waiting:
	case err := <-result:
		t.Fatalf("财报提交没有核验并等待来源行锁：%v", err)
	case <-ctx.Done():
		t.Fatal("财报提交未进入来源行锁")
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("来源发布提交后提醒评估没有结束")
	}
	var count int64
	if err := db.Model(&model.AlertEvent{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("来源已发布后不能有旧预约事件：count=%d err=%v", count, err)
	}
}

func TestAlertFinancialReviewRevisedForecastIsNewFact(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940014
	rule := model.AlertRule{UserID: userID, Symbol: "600014", Market: "cn", Kind: model.AlertKindEarnFcst, Op: model.AlertOpGTE, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	forecast := model.EarningsForecast{Symbol: rule.Symbol, Market: "cn", ReportDate: "2026-06-30", NoticeDate: time.Now().Format("2006-01-02"), PredictType: "预增", PredictFinance: "净利润", AmpLower: 20, AmpUpper: 30}
	if err := common.DB.Create(&forecast).Error; err != nil {
		t.Fatal(err)
	}
	svc := &AlertService{}
	for i, want := range []int{1, 0, 1, 0} {
		if i == 2 {
			if err := common.DB.Model(&forecast).Updates(map[string]any{"predict_type": "预减", "amp_lower": -30, "amp_upper": -20}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if hits, err := svc.evaluateEarnRulesForUserContext(t.Context(), userID); err != nil || hits != want {
			t.Errorf("第 %d 次评估：同日预告修正应产生新事实，其余重复不应新增；hits=%d want=%d err=%v", i+1, hits, want, err)
		}
	}
}

func TestAlertFinancialReviewChangedSourceCannotCommitOldFact(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940015
	rule := model.AlertRule{UserID: userID, Symbol: "600015", Market: "cn", Kind: model.AlertKindEarnDate, Op: model.AlertOpGTE, Threshold: 3, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	schedule := model.DisclosureSchedule{Symbol: rule.Symbol, Market: "cn", ReportDate: "2026-06-30", AppointDate: time.Now().AddDate(0, 0, 1).Format("2006-01-02")}
	if err := common.DB.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	const hook = "review_alert_financial_source_changed"
	if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*model.DisclosureSchedule); ok && row.ID == schedule.ID && !changed {
			changed = true
			if err := common.DB.Model(&schedule).Update("is_published", true).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	hits, err := (&AlertService{}).evaluateEarnRulesForUserContext(t.Context(), userID)
	if err != nil || !changed || hits != 0 {
		t.Errorf("读取后已经发布的报告不能再提交即将披露提醒：changed=%v hits=%d err=%v", changed, hits, err)
	}
	var count int64
	if err := common.DB.Model(&model.AlertEvent{}).Where("rule_id = ?", rule.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("提交了 %d 条已经失效的财报事实", count)
	}
}
