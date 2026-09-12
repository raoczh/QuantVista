package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestQualityReviewIncompleteCalendarCannotHideStaleBars(t *testing.T) {
	setupSelectionEvalTestDB(t)
	for _, date := range []string{"2026-06-01", "2026-06-02"} {
		mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true})
	}
	candidate := qgFullCandidate()
	candidate.lastBarDate = "2026-06-01"
	quality := computeQualityGate(model.RecTypeShortTerm, candidate, "2026-06-12", recentOpenDays("2026-06-12", 45))
	if quality == nil || quality.WouldBeConfidenceCap != qgCapStaleBars || quality.DataAgeTradeDays != -1 {
		t.Fatalf("日历只到 6 月 2 日，不能把 11 天旧日线判作只漏一天：%+v", quality)
	}
}

func TestQualityReviewCompleteHolidayCalendarKeepsFreshBars(t *testing.T) {
	setupSelectionEvalTestDB(t)
	end := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 45; i++ {
		day := end.AddDate(0, 0, -i)
		date := day.Format("2006-01-02")
		isOpen := (date <= "2026-02-13" || date == "2026-02-24") && day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
		mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: isOpen})
	}
	candidate := qgFullCandidate()
	candidate.lastBarDate = "2026-02-13"
	if got := computeQualityGate(model.RecTypeShortTerm, candidate, "2026-02-24", recentOpenDays("2026-02-24", 45)); got != nil {
		t.Fatalf("完整日历确认春节期间休市，节前日线仍可接受：%+v", got)
	}
}

func TestShadowReviewForcedPricesAreNotEvaluationSamples(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	seedShadowLabel(t, 2211, 0, 2211, "600901", model.RecActionBuy, model.LabelMatured, 2, 1)
	seedShadowLabel(t, 2212, 0, 2212, "600902", model.RecActionBuy, model.LabelMatured, -90, -91)
	seedShadowEvent(t, 2213, "600903", model.CandStagePicked, model.GateQualityShadow, "", "")
	seedShadowLabel(t, 2213, 0, 2213, "600903", model.RecActionBuy, model.LabelMatured, 3, 2)
	seedShadowEvent(t, 2214, "600904", model.CandStagePicked, model.GateQualityShadow, "", "")
	seedShadowLabel(t, 2214, 0, 2214, "600904", model.RecActionBuy, model.LabelMatured, -80, -81)
	if err := common.DB.Model(&model.RecommendationLabel{}).Where("recommendation_id IN ?", []int64{2212, 2214}).Update("forced", true).Error; err != nil {
		t.Fatal(err)
	}
	report, err := RecShadowReport(11, model.RecTypeShortTerm, 10)
	if err != nil || report == nil || len(report.Groups) != 1 {
		t.Fatalf("影子报表读取失败：%v %+v", err, report)
	}
	group := report.Groups[0]
	if group.Gated.Sample != 1 || group.Gated.AvgNetPct != 3 || group.Ungated.Sample != 1 || group.Ungated.AvgNetPct != 2 {
		t.Fatalf("退市/长停的强制估值不能作为真实可成交收益评估门控：%+v", group)
	}
	if group.Marked != 2 || report.PickedBuy != 4 || report.ForcedExcluded != 2 {
		t.Fatalf("质量剔除仍应保留真实的名单覆盖分母：%+v", report)
	}
}
