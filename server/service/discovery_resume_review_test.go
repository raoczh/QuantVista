package service

import (
	"errors"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestDiscoveryMissingScoreFactorIsNotReady(t *testing.T) {
	table := discoveryTestTable("2026-09-10", "600061", "600062")
	table.cols["vol_boost"][0] = math.NaN()
	rows := BuildDiscoverySignals(table, 100)["trend_breakout"]
	valid := false
	for _, row := range rows {
		if row.Symbol == "600061" && row.DataStatus == DiscoveryItemReady {
			t.Errorf("缺少必要评分因子不能当零值计算并标ready：%+v", row)
		}
		if row.Symbol == "600062" {
			valid = true
		}
	}
	if !valid {
		t.Fatal("同通道完整数据的候选仍应保留")
	}
}

func TestDiscoveryHistoryFailureIsNotFirstAppearance(t *testing.T) {
	setupTestDB(t)
	date := time.Now().Format("2006-01-02")
	addDiscoveryCalendar(t, date)
	installDiscoveryTestTable(t, discoveryTestTable(date))
	forced := errors.New("discovery history unavailable")
	const callback = "review_discovery_history_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "candidate_discovery_items" {
			tx.AddError(forced)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	if _, err := ExecuteDailyDiscovery(t.Context(), 0, "cn", date, discoveryParameterHash()); !errors.Is(err, forced) {
		t.Fatalf("历史读取失败不能生成伪造的首日排名和连续天数：%v", err)
	}
}

func TestRecentDiscoveryCannotSelectFutureRun(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	future := mustAddDays(today, 7)
	addDiscoveryCalendar(t, today, future)
	for i, date := range []string{today, future} {
		now := time.Now()
		run := model.CandidateDiscoveryRun{Market: "cn", TradeDate: date, DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion,
			ParameterHash: discoveryParameterHash(), Status: DiscoveryRunStatusOK, StartedAt: now, AsOf: now, FinishedAt: &now}
		if err := common.DB.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		row := model.CandidateDiscoveryItem{RunID: run.ID, Market: "cn", TradeDate: date, DiscoveryVersion: DiscoveryVersion,
			Channel: discoveryChannels[0], Symbol: []string{"600061", "600062"}[i], Rank: 1, Score: 80, DataStatus: DiscoveryItemReady, AsOf: now}
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows := recentDiscoveryCandidates("cn", 100)
	found := false
	for _, row := range rows {
		if row.candidate.Symbol == "600062" {
			t.Error("未来发现运行不能进入今天推荐的发现记忆")
		}
		if row.candidate.Symbol == "600061" {
			found = true
		}
	}
	if !found {
		t.Fatal("当天真实发现应继续保留")
	}
}
