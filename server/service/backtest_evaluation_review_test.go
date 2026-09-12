package service

import (
	"context"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestWalkForwardReviewForcedMemberIsNotNormalTrade(t *testing.T) {
	setupTestDB(t)
	seedWFFixture(t)
	if err := common.DB.Where("symbol = ? AND trade_date > ?", "600100", "2026-03-03").Delete(&model.DailyBar{}).Error; err != nil {
		t.Fatal(err)
	}
	report, err := RunWalkForward(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range report.Sections {
		if section.RecType != model.RecTypeShortTerm {
			continue
		}
		for _, month := range section.Monthly {
			if month.Month != "2026-03" || month.Strategy != "momentum" {
				continue
			}
			for _, item := range month.Items {
				if item.Symbol != "600100" {
					continue
				}
				if month.Forced != 1 || item.Status != "forced" || item.NetPct != nil || item.AlphaPct != nil {
					t.Fatalf("已从统计剔除的长停强平，明细不能仍是普通成交收益：forced=%d item=%+v", month.Forced, item)
				}
				return
			}
		}
	}
	t.Fatal("未形成包含长停标的的三月组合")
}

func TestBacktestReviewEqualLiquidityRankingIsStable(t *testing.T) {
	first := btCandidate{Symbol: "600001", SignalDate: "2026-06-01", AmountYi: 1, Holds: map[int]holdOutcome{5: {Status: btTraded, ReturnPct: 10}}}
	second := btCandidate{Symbol: "600002", SignalDate: "2026-06-01", AmountYi: 1, Holds: map[int]holdOutcome{5: {Status: btTraded, ReturnPct: -10}}}
	tree := allOf(leafV("close", ">", 5))
	for _, candidates := range [][]btCandidate{{first, second}, {second, first}} {
		report := (&BacktestService{}).aggregate(&tree, "本地排序审查", candidates, []int{5}, []string{"2026-06-01"}, 1, nil)
		if len(report.Stats) != 1 || report.Stats[0].AvgReturnPct != 10 {
			t.Fatalf("同金额候选的结果不能由 worker 返回顺序决定：%+v", report.Stats)
		}
	}
}
