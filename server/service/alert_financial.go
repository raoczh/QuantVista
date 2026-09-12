package service

import (
	"errors"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type alertFinancialEvaluation struct {
	triggered bool
	value     float64
	message   string
	context   *AlertEventContext
}

// 先判断当前事实是否满足条件；是否已经提醒过由提交事务在规则锁内核对。
func readAlertFinancialEvaluation(db *gorm.DB, rule model.AlertRule, now time.Time, lock bool) (alertFinancialEvaluation, error) {
	var out alertFinancialEvaluation
	if rule.Market != "cn" {
		return out, errors.New("财报类提醒仅支持 A 股")
	}
	if lock && db.Dialector.Name() != "sqlite" {
		db = db.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	today := now.In(time.Local).Format("2006-01-02")
	rule.TriggeredAt = nil
	switch rule.Kind {
	case model.AlertKindEarnDate:
		var schedule model.DisclosureSchedule
		err := db.Where("symbol = ? AND market = ? AND appoint_date >= ? AND is_published = ?", rule.Symbol, rule.Market, today, false).
			Order("appoint_date ASC, report_date DESC, id DESC").First(&schedule).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.triggered, out.value, out.message = evaluateEarnDate(rule, schedule.AppointDate, schedule.ReportTypeName, now)
		if out.triggered {
			out.context = buildEarnDateAlertContext(rule, schedule, out.value, out.message)
		}
	case model.AlertKindEarnFcst:
		var forecast model.EarningsForecast
		err := db.Where("symbol = ? AND market = ? AND notice_date <= ?", rule.Symbol, rule.Market, today).
			Order("notice_date DESC, id DESC").First(&forecast).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.triggered, out.value, out.message = evaluateEarnFcst(rule, &forecast, now)
		if out.triggered {
			out.context = buildEarnForecastAlertContext(rule, forecast, out.value, out.message)
		}
	}
	return out, nil
}

func sameAlertFinancialFact(a, b *AlertEventContext) bool {
	if a == nil || b == nil || a.Financial == nil || b.Financial == nil || a.Rule.Kind != b.Rule.Kind {
		return false
	}
	left, right := a.Financial, b.Financial
	if left.FactType != right.FactType || left.ReportDate != right.ReportDate {
		return false
	}
	if left.FactType == "disclosure_schedule" {
		return left.AppointDate == right.AppointDate
	}
	if left.FactType != "earnings_forecast" {
		return false
	}
	equalNumber := func(a, b *float64) bool {
		return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
	}
	return left.NoticeDate == right.NoticeDate && left.PredictType == right.PredictType &&
		left.PredictFinance == right.PredictFinance && equalNumber(left.AmpLower, right.AmpLower) && equalNumber(left.AmpUpper, right.AmpUpper)
}

// 只比较最近一次已交付的事实，定期刷新相同数据不重复提醒；事实改变或恢复原预约日均可重新提醒。
func alertFinancialFactAlreadySeen(tx *gorm.DB, rule model.AlertRule, next *AlertEventContext, now time.Time) (bool, error) {
	var event model.AlertEvent
	err := tx.Where("rule_id = ? AND user_id = ? AND kind = ? AND position_id = 0", rule.ID, rule.UserID, rule.Kind).
		Order("id DESC").First(&event).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	if previous, ok := parseAlertEventContext(event.ContextVersion, event.ContextJSON); ok && previous.Financial != nil {
		return sameAlertFinancialFact(previous, next), nil
	}
	// 没有结构化快照的旧规则保留原来的窗口/发布日期去重，不猜测或回写旧事件。
	fact := next.Financial
	if rule.Kind == model.AlertKindEarnDate {
		hit, _, _ := evaluateEarnDate(rule, fact.AppointDate, fact.ReportType, now)
		return !hit, nil
	}
	hit, _, _ := evaluateEarnFcst(rule, &model.EarningsForecast{NoticeDate: fact.NoticeDate}, now)
	return !hit, nil
}
