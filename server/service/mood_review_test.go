package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestMoodReviewLhbSelectionUsesUnroundedAmount(t *testing.T) {
	setupTestDB(t)
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	pinCalendarTo(t, day)
	rows := []model.LhbEntry{
		{Market: "cn", Symbol: "600519", TradeDate: day, ChangeType: "buy", NetBuy: 1490000, Reason: "较大净买入"},
		{Market: "cn", Symbol: "600519", TradeDate: day, ChangeType: "sell", NetBuy: -1200000, Reason: "较小净卖出"},
	}
	if err := common.DB.Create(rows).Error; err != nil {
		t.Fatal(err)
	}
	sig := lhbSignalsFor(t.Context(), []string{"600519"})["600519"]
	if sig.NetBuyYi <= 0 || sig.Reason != "较大净买入" {
		t.Fatalf("先比较原始绝对净额再舍入，否则会把较小卖出误选为主信号：%+v", sig)
	}
}

func TestMoodReviewRejectsFutureSignalDate(t *testing.T) {
	setupTestDB(t)
	if signalDateUsable(time.Now().AddDate(0, 0, 1).Format("2006-01-02")) {
		t.Fatal("未来日期不能作为当前可用信号")
	}
}

func TestMoodReviewHistoricalNotReadyPreservesKnownInstitution(t *testing.T) {
	setupTestDB(t)
	const day = "2026-09-08"
	if err := common.DB.Create(&model.LhbEntry{Market: "cn", Symbol: "600519", TradeDate: day, ChangeType: "old", Name: "旧主榜"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.LhbOrgDaily{Market: "cn", Symbol: "600519", TradeDate: day, NetBuy: 10000000}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &MoodService{
		now: func() time.Time { return time.Date(2026, 9, 9, 1, 0, 0, 0, time.Local) },
		fetchLhbDaily: func(context.Context, string) ([]datasource.LhbRow, error) {
			return []datasource.LhbRow{{Symbol: "600000", ChangeType: "new", Name: "新主榜"}}, nil
		},
		fetchLhbOrgDaily: func(context.Context, string) ([]datasource.LhbOrgRow, error) { return nil, datasource.ErrLhbNotReady },
	}
	if _, err := svc.SyncLhb(t.Context(), day); !errors.Is(err, datasource.ErrLhbNotReady) {
		t.Errorf("未就绪与已有机构证据矛盾时须保留旧快照并重试：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.LhbOrgDaily{}).Where("trade_date = ? AND net_buy = ?", day, 10000000).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("上游历史 9201 抹掉了已有机构数据：count=%d", count)
	}
}

func TestMoodReviewGapReadFailureCannotClaimCaughtUp(t *testing.T) {
	setupTestDB(t)
	resetLhbGapState(t)
	const target = "2026-09-08"
	if err := model.UpsertOption(optMoodLhbDay, target); err != nil {
		t.Fatal(err)
	}
	if err := saveLhbGaps([]string{"2026-09-01"}); err != nil {
		t.Fatal(err)
	}
	const hook = "review_lhb_gap_read_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "options" && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"].Expression), optMoodLhbGaps) {
			tx.AddError(errors.New("模拟缺口清单读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	if NewMoodService().runMoodLhb(t.Context(), target) {
		t.Fatal("缺口清单不可读时不能宣称龙虎榜已追平")
	}
}

func TestMoodReviewGapRetryDoesNotStarveLaterDates(t *testing.T) {
	setupTestDB(t)
	resetLhbGapState(t)
	dates := []string{"2026-08-24", "2026-08-25", "2026-08-26", "2026-08-27", "2026-08-28", "2026-08-31"}
	calls := map[string]int{}
	svc := &MoodService{fetchLhbDaily: func(_ context.Context, day string) ([]datasource.LhbRow, error) {
		calls[day]++
		return nil, errors.New("持续不可用")
	}}
	for i := 0; i < 3; i++ {
		svc.retryLhbGaps(t.Context(), dates)
	}
	if calls[dates[5]] == 0 {
		t.Fatalf("前五天持续失败不能使第六个缺口永远没有重试机会：%v", calls)
	}
}

func TestMoodReviewPopularityReplacesWholeDailyMembership(t *testing.T) {
	setupTestDB(t)
	day := time.Now().Format("2006-01-02")
	if err := common.DB.Create(&model.PopularityRank{Market: "cn", Symbol: "600519", TradeDate: day, Rank: 1}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &MoodService{fetchPopularity: func(context.Context) ([]datasource.PopularityRow, error) {
		return []datasource.PopularityRow{{Market: "cn", Symbol: "600000", Rank: 1, PrevRank: 2}}, nil
	}}
	if err := svc.SyncPopularity(t.Context(), day); err != nil {
		t.Fatal(err)
	}
	var rows []model.PopularityRank
	if err := common.DB.Where("trade_date = ?", day).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Symbol != "600000" {
		t.Fatalf("重采后不得保留已退出榜单的旧股票：%+v", rows)
	}
}

func TestMoodReviewPopularityFailureRollsBackReplacement(t *testing.T) {
	setupTestDB(t)
	day := time.Now().Format("2006-01-02")
	if err := common.DB.Create(&model.PopularityRank{Market: "cn", Symbol: "600519", TradeDate: day, Rank: 1}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &MoodService{fetchPopularity: func(context.Context) ([]datasource.PopularityRow, error) {
		return []datasource.PopularityRow{{Market: "cn", Symbol: "600000", Rank: 1}, {Market: "cn", Symbol: "600000", Rank: 2}}, nil
	}}
	if err := svc.SyncPopularity(t.Context(), day); err == nil {
		t.Error("无效重复名单不应提交")
	}
	var rows []model.PopularityRank
	if err := common.DB.Where("trade_date = ?", day).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Symbol != "600519" {
		t.Fatalf("替换失败后应保留原名单：%+v", rows)
	}
}

func TestMoodReviewPoolRetryStaysInAvailableWindow(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 9, 9, 17, 0, 0, 0, time.Local)
	if err := model.UpsertOption(optMoodPoolDay, "2026-09-09"); err != nil {
		t.Fatal(err)
	}
	if delay := nextMoodPoolDelay(now); delay > lhbRetryInterval {
		t.Fatalf("当日人气榜尚未完成，不能等到次日而错过不可回溯窗口：delay=%v", delay)
	}
	if err := model.UpsertOption(optMoodPopDay, "2026-09-09"); err != nil {
		t.Fatal(err)
	}
	if delay := nextMoodPoolDelay(now); delay != nextDailyAt(now, 16, 35).Sub(now) {
		t.Fatalf("两份已完成才等待下个日程：delay=%v", delay)
	}
}
