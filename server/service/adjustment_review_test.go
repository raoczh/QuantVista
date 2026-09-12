package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedAdjustmentReviewBars(t *testing.T, symbol string, mixed bool) []datasource.Bar {
	t.Helper()
	bars := wideGenBars(wideGenDates(40, time.Now().AddDate(0, 0, -2)), 10)
	rows := make([]model.DailyBar, 0, len(bars))
	for i := range bars {
		bars[i].Source = "eastmoney"
		if mixed && i == 20 {
			bars[i].Source = "sina" // 价格恰好相同，不能依靠涨跌幅断层检测来识别来源。
		}
		b := bars[i]
		rows = append(rows, model.DailyBar{Market: "cn", Symbol: symbol, TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Amount: b.Amount,
			TurnoverRate: b.TurnoverRate, Source: b.Source})
	}
	if err := common.DB.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, Name: "普通股票", InitStatus: "done",
		BarsCount: len(bars), LastBarDate: bars[len(bars)-1].TradeDate}).Error; err != nil {
		t.Fatal(err)
	}
	return bars
}

func TestLocalMixedHistoryCannotFeedFactorsAndReturns(t *testing.T) {
	setupTestDB(t)
	seedAdjustmentReviewBars(t, "600081", true)
	seedAdjustmentReviewBars(t, "600082", false)
	table, err := buildFactorTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if table.Len() != 2 || table.Fresh(0) || !table.Fresh(1) || !math.IsNaN(table.Col("close")[0]) || table.FreshCoverage != 0.5 {
		t.Fatalf("问题序列不能视为新鲜有效因子，覆盖分母仍应保留：%+v", table)
	}
	if inserted, err := SnapshotFactorTable(table); err != nil || inserted != 1 {
		t.Fatalf("只可固化可靠序列：inserted=%d err=%v", inserted, err)
	}
	if _, err := cnDailyBarsAsc(context.Background(), "600081"); !errors.Is(err, errUnadjustedBars) {
		t.Fatalf("标签/选择评估必须收到明确来源错误：%v", err)
	}
	if _, err := localBarReturns("600081", "cn", 40, table.TradeDate); !errors.Is(err, errUnadjustedBars) {
		t.Fatalf("组合风险不能使用混源收益：%v", err)
	}
	if _, err := recentLocalBars(context.Background(), "cn", "600081", 40); !errors.Is(err, errUnadjustedBars) {
		t.Fatalf("卖出复核不能使用混源日线：%v", err)
	}
	seen := false
	if err := streamCNDailyBars(context.Background(), func(symbol string, bars []datasource.Bar) {
		if symbol == "600081" {
			seen = true
			if !adjustSuspect(bars, symbol, "普通股票") {
				t.Error("流式 IC/回测/召回读取必须保留来源并识别问题序列")
			}
		}
	}); err != nil || !seen {
		t.Fatalf("流式来源回归未执行：seen=%v err=%v", seen, err)
	}
}

func TestMixedHistoryRepairKeepsOldSnapshotUnverified(t *testing.T) {
	setupTestDB(t)
	const symbol = "600083"
	bars := seedAdjustmentReviewBars(t, symbol, true)
	snapshot := model.FactorSnapshotDaily{Market: "cn", Symbol: symbol, TradeDate: bars[len(bars)-1].TradeDate,
		LastBarDate: bars[len(bars)-1].TradeDate, FactorsJSON: `{"close":99}`, FactorVersion: "fv2"}
	if err := common.DB.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	var usable int64
	if err := usableFactorSnapshots(common.DB).Model(&model.FactorSnapshotDaily{}).Count(&usable).Error; err != nil || usable != 0 {
		t.Fatalf("修复前已知混源的旧快照不能参与评估：usable=%d err=%v", usable, err)
	}
	full := wideGenBars(wideGenDates(250, time.Now().AddDate(0, 0, -2)), 10)
	fake := &fakeWideSource{bars: map[string][]datasource.Bar{symbol: full}}
	fresh := append([]datasource.Bar(nil), full[len(full)-3:]...)
	for i := range fresh {
		fresh[i].Source = "eastmoney"
	}
	if err := (&MarketService{wide: fake}).persistDailyBars(context.Background(), "cn", symbol, fresh); err != nil {
		t.Fatal(err)
	}
	if fake.barsCalls[symbol] != 1 || barCount(t, symbol) != 250 {
		t.Fatal("尾窗价格一致也必须完整替换存量不复权序列")
	}
	var stored model.FactorSnapshotDaily
	if err := common.DB.First(&stored, snapshot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.FactorsJSON != snapshot.FactorsJSON || stored.FactorVersion != "fv2" || stored.DataQuality != model.FactorQualityUnverifiedAdjustment {
		t.Fatalf("旧快照仅补独立质量标记，不改写因子或原版本：%+v", stored)
	}
	if err := usableFactorSnapshots(common.DB).Model(&model.FactorSnapshotDaily{}).Count(&usable).Error; err != nil || usable != 0 {
		t.Fatalf("清理旧日线后不得重新信任问题快照：usable=%d err=%v", usable, err)
	}
}

func TestWideRebaseFailureDoesNotAppendNewBasis(t *testing.T) {
	setupTestDB(t)
	factorBuildMu.Lock() // 本用例只验证同步写入，阻止旁路后台构建。
	defer factorBuildMu.Unlock()
	snapshot, date := wideTestSnapshot()
	old := model.DailyBar{Market: "cn", Symbol: "600001", TradeDate: "2026-07-06", Open: 13, High: 13, Low: 13, Close: 13, Source: "eastmoney"}
	if err := common.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: old.Symbol, InitStatus: "done", LastBarDate: old.TradeDate}).Error; err != nil {
		t.Fatal(err)
	}
	fake := &fakeWideSource{snapshot: snapshot, barsErr: map[string]error{old.Symbol: errors.New("重锚源暂不可用")}}
	log, err := (&MarketService{wide: fake}).SyncMarketWide(context.Background())
	if err != nil || log.Failed != 1 || log.Succeeded != 1 {
		t.Fatalf("应仅完成另一只股票：log=%+v err=%v", log, err)
	}
	var count int64
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", old.Symbol, date).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("重锚失败不得拼入新基准日线：count=%d err=%v", count, err)
	}
	state := stateOf(t, old.Symbol)
	if state.InitStatus != "pending" || state.LastBarDate != old.TradeDate {
		t.Fatalf("未写入的日线不能推进数据日期：%+v", state)
	}
}

func TestHistoryInitDoesNotHideStateFailures(t *testing.T) {
	setupTestDB(t)
	for _, phase := range []string{"count", "update"} {
		t.Run(phase, func(t *testing.T) {
			cleanWideTables(t)
			const symbol = "600084"
			if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
				t.Fatal(err)
			}
			fault := errors.New("同步状态存储暂时故障")
			inject := func(tx *gorm.DB) {
				if tx.Statement.Table == "market_sync_states" {
					tx.AddError(fault)
				}
			}
			const callback = "review_history_state_failure"
			if phase == "count" {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, inject); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			} else {
				if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, inject); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
			}
			fake := &fakeWideSource{bars: map[string][]datasource.Bar{symbol: wideGenBars([]string{"2026-09-01"}, 10)}}
			log, err := (&MarketService{wide: fake}).initMarketWideHistory(context.Background())
			if !errors.Is(err, fault) || log == nil || log.Status != "failed" || log.Succeeded != 0 {
				t.Fatalf("状态查询/写入失败不能伪装成无待处理或初始化成功：log=%+v err=%v", log, err)
			}
		})
	}
}

func TestHistoryInitCancellationDuringWriteKeepsRetryBudget(t *testing.T) {
	setupTestDB(t)
	// database/sql 会丢弃取消事务的连接；保留一个独立连接，避免最后一个
	// SQLite 内存连接被关闭后整个测试库消失，掩盖要验证的事务回滚行为。
	sqlDB, err := common.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	const symbol = "600085"
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callback = "review_history_cancel_during_write"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_bars" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
	fake := &fakeWideSource{bars: map[string][]datasource.Bar{symbol: wideGenBars([]string{"2026-09-01"}, 10)}}
	log, err := (&MarketService{wide: fake}).initMarketWideHistory(ctx)
	if !errors.Is(err, context.Canceled) || log.Failed != 0 || log.Succeeded != 0 {
		t.Fatalf("取消不能消耗标的失败重试预算：log=%+v err=%v", log, err)
	}
	if st := stateOf(t, symbol); st.FailCount != 0 || st.InitStatus != "pending" {
		t.Fatalf("取消后应保持待重试状态：%+v", st)
	}
}

func TestRetentionKeepsMixedHistoryEvidence(t *testing.T) {
	setupTestDB(t)
	const symbol = "600086"
	bars := seedAdjustmentReviewBars(t, symbol, true)
	snapshot := model.FactorSnapshotDaily{Market: "cn", Symbol: symbol, TradeDate: bars[len(bars)-1].TradeDate,
		LastBarDate: bars[len(bars)-1].TradeDate, FactorsJSON: `{"close":99}`, FactorVersion: "fv2"}
	if err := common.DB.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupDailyBarsBefore(bars[21].TradeDate); err != nil {
		t.Fatal(err)
	}
	var usable int64
	if err := usableFactorSnapshots(common.DB).Model(&model.FactorSnapshotDaily{}).Count(&usable).Error; err != nil || usable != 0 {
		t.Fatalf("保留期清理不能洗掉旧因子快照的混源证据：usable=%d err=%v", usable, err)
	}
	var stored model.FactorSnapshotDaily
	if err := common.DB.First(&stored, snapshot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.FactorsJSON != snapshot.FactorsJSON || stored.FactorVersion != snapshot.FactorVersion || stored.DataQuality != model.FactorQualityUnverifiedAdjustment {
		t.Fatalf("清理应保留因子原值与版本，并补质量审计标记：%+v", stored)
	}
}

func TestRetentionMetadataFailureKeepsBars(t *testing.T) {
	setupTestDB(t)
	const symbol = "600089"
	bars := seedAdjustmentReviewBars(t, symbol, true)
	fault := errors.New("质量审计标记写入失败")
	const callback = "review_retention_metadata_failure"
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "factor_snapshot_dailies" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
	if deleted, err := CleanupDailyBarsBefore(bars[21].TradeDate); deleted != 0 || !errors.Is(err, fault) {
		t.Fatalf("质量记录失败时本批删除必须回滚：deleted=%d err=%v", deleted, err)
	}
	if count := barCount(t, symbol); count != int64(len(bars)) {
		t.Fatalf("原始来源证据必须保留：count=%d want=%d", count, len(bars))
	}
}

func TestDailyWindowCannotCrossRebase(t *testing.T) {
	setupTestDB(t)
	checkDailyWindowCannotCrossRebase(t)
}

func checkDailyWindowCannotCrossRebase(t *testing.T) {
	t.Helper()
	const symbol = "600090"
	old := seedAdjustmentReviewBars(t, symbol, false)
	fresh := wideGenBars(wideGenDates(250, time.Now().AddDate(0, 0, -2)), 5)
	for i := range fresh {
		fresh[i].Source = "eastmoney"
	}
	svc := &MarketService{}
	called := false
	var rebaseErr error
	const callback = "review_rebase_after_daily_validation"
	db := common.DB
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_bars" && !called {
			called = true
			// 模拟窗口 A 已完成复权校验、尚未写入时，另一个事务 B 重建整段历史。
			rebaseErr = svc.rebaseStock(context.Background(), "cn", symbol, fresh)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Create().Remove(callback) })
	if err := svc.persistDailyBars(context.Background(), "cn", symbol, old[len(old)-3:]); err != nil {
		t.Fatal(err)
	}
	if !called || (rebaseErr != nil && !errors.Is(rebaseErr, errRebaseInProgress)) {
		t.Fatalf("并发提交交错未执行：called=%v err=%v", called, rebaseErr)
	}
	var prices []float64
	if err := db.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).Distinct("close").Pluck("close", &prices).Error; err != nil {
		t.Fatal(err)
	}
	if len(prices) != 1 {
		t.Fatalf("检测与写入之间不得插入重锚事务，留下两种价格基准：%v", prices)
	}
	if rebaseErr != nil {
		if err := svc.rebaseStock(context.Background(), "cn", symbol, fresh); err != nil {
			t.Fatalf("并发请求应在原写入完成后可重试：%v", err)
		}
	}
}

func TestWideIncrementRechecksBasisAfterInitialScan(t *testing.T) {
	setupTestDB(t)
	factorBuildMu.Lock()
	defer factorBuildMu.Unlock()
	snapshot, date := wideTestSnapshot()
	const symbol = "600001"
	if err := common.DB.Create(&model.DailyBar{Market: "cn", Symbol: symbol, TradeDate: "2026-07-06",
		Open: 10, High: 10, Low: 10, Close: 10, Source: "eastmoney"}).Error; err != nil {
		t.Fatal(err)
	}
	prior, _ := time.ParseInLocation("2006-01-02", "2026-07-06", time.Local)
	fresh := wideGenBars(wideGenDates(250, prior), 5)
	for i := range fresh {
		fresh[i].Source = "eastmoney"
	}
	svc := &MarketService{wide: &fakeWideSource{snapshot: snapshot}}
	called := false
	var rebaseErr error
	const callback = "review_wide_after_initial_basis_scan"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if !called && tx.Statement.Table == "daily_bars" && strings.Contains(tx.Statement.SQL.String(), "DISTINCT") {
			called = true
			rebaseErr = svc.rebaseStock(context.Background(), "cn", symbol, fresh)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 函数返回先释放构建锁；后台查询结束后才能修改 GORM 回调表。
		waitForTestFactorRebuilds(t)
		common.DB.Callback().Query().Remove(callback)
	})
	log, err := svc.SyncMarketWide(context.Background())
	if err != nil || !called || rebaseErr != nil || log.Succeeded != 1 || log.Failed != 1 {
		t.Fatalf("锁内复验应隔离初筛后基准变化的股票：log=%+v called=%v rebase=%v err=%v", log, called, rebaseErr, err)
	}
	var count int64
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND trade_date = ?", "cn", symbol, date).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("旧快照价不能拼入新基准历史：count=%d err=%v", count, err)
	}
	if state := stateOf(t, symbol); state.InitStatus != "pending" || state.LastBarDate != "2026-07-06" {
		t.Fatalf("未提交的当日行情不得推进水位：%+v", state)
	}
}

func TestGapFillKeepsOverlappingAdjustmentAnchors(t *testing.T) {
	setupTestDB(t)
	const symbol = "600091"
	old := seedAdjustmentReviewBars(t, symbol, false)
	missingDate := old[20].TradeDate
	if err := common.DB.Where("market = ? AND symbol = ? AND trade_date = ?", "cn", symbol, missingDate).Delete(&model.DailyBar{}).Error; err != nil {
		t.Fatal(err)
	}
	fresh := wideGenBars(wideGenDates(250, time.Now().AddDate(0, 0, -2)), 5)
	var missing []datasource.Bar
	for i := range fresh {
		fresh[i].Source = "eastmoney"
		if fresh[i].TradeDate == missingDate {
			missing = append(missing, fresh[i])
		}
	}
	if len(missing) != 1 {
		t.Fatal("测试缺口必须位于上游窗口内")
	}
	if err := (&MarketService{}).persistDailyBarsChecked(context.Background(), "cn", symbol, missing, fresh, false); err != nil {
		t.Fatal(err)
	}
	var prices []float64
	if err := common.DB.Model(&model.DailyBar{}).Where("market = ? AND symbol = ?", "cn", symbol).Distinct("close").Pluck("close", &prices).Error; err != nil || len(prices) != 1 || prices[0] != 5 {
		t.Fatalf("只写缺口也必须利用完整重叠窗口识别复权变化：prices=%v err=%v", prices, err)
	}
}

func TestSingleDayWriteCannotBypassAdjustmentCheck(t *testing.T) {
	setupTestDB(t)
	const symbol = "600101"
	bars := seedAdjustmentReviewBars(t, symbol, false)
	today := time.Now().Format("2006-01-02")
	fresh := []datasource.Bar{{TradeDate: today, Open: 5, High: 5, Low: 5, Close: 5, Source: "eastmoney"}}
	if err := (&MarketService{}).persistDailyBars(context.Background(), "cn", symbol, fresh); err == nil {
		t.Fatal("只含当日一根、无法校验复权的窗口不能拼入历史")
	}
	if count := barCount(t, symbol); count != int64(len(bars)) {
		t.Fatalf("缺少锚点时应保留原有日线：count=%d want=%d", count, len(bars))
	}
}

func TestOldMixedPeakCannotProduceDrawdown(t *testing.T) {
	setupTestDB(t)
	const symbol = "600093"
	bars := seedAdjustmentReviewBars(t, symbol, true)
	p := seedHoldingWithPeak(t, 991, symbol, "旧峰值核验", 10, 100, 20, bars[0].TradeDate)
	p.PeakDate, p.PeakBackfilled = bars[20].TradeDate, true
	if err := common.DB.Save(p).Error; err != nil {
		t.Fatal(err)
	}
	positions := []model.Position{*p}
	before := time.Now().Format("2006-01-02")
	if _, err := syncPositionPeaksBefore(context.Background(), p.UserID, positions, before); err != nil {
		t.Fatal(err)
	}
	view := peakViewFor(positions[0], 10, 10, before)
	if view == nil || view.DrawdownPct != 0 || !strings.Contains(view.Note, "待核验") {
		t.Fatalf("旧混源派生峰值不能继续展示确定的回撤：%+v", view)
	}
	fresh := wideGenBars(wideGenDates(250, time.Now().AddDate(0, 0, -2)), 10)
	for i := range fresh {
		fresh[i].Source = "eastmoney"
	}
	if err := (&MarketService{}).rebaseStock(context.Background(), "cn", symbol, fresh); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.First(p, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if p.PeakPrice != 20 || trustedPositionPeak(*p) != 0 {
		t.Fatalf("重建日线后仍须保留旧峰值原值及其待核验状态：%+v", p)
	}
	resetPeakOnBuy(p, 9, before, before)
	if view := peakViewFor(*p, 9, 9, before); view == nil || view.Price != 9 || view.DataQuality != "" {
		t.Fatalf("明确发生新买入后，应使用本次成交重新起算峰值：%+v", view)
	}
}

func TestHistoryInitFailureCannotUndoConcurrentCompletion(t *testing.T) {
	setupTestDB(t)
	const symbol = "600095"
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol, InitStatus: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	var completionErr error
	fake := &fakeWideSource{barsErr: map[string]error{symbol: datasource.ErrNoData}, onBars: func(string, int) {
		completionErr = common.DB.Model(&model.MarketSyncState{}).Where("market = ? AND symbol = ?", "cn", symbol).
			Updates(map[string]any{"init_status": "done", "bars_count": 250, "last_bar_date": "2026-09-07", "fail_count": 0}).Error
	}}
	log, err := (&MarketService{wide: fake}).initMarketWideHistory(context.Background())
	if err != nil || completionErr != nil || log.Failed != 0 {
		t.Fatalf("旧请求失败不应覆盖另一条链路已完成的状态：log=%+v completion=%v err=%v", log, completionErr, err)
	}
	if state := stateOf(t, symbol); state.InitStatus != "done" || state.FailCount != 0 || state.BarsCount != 250 {
		t.Fatalf("并发完成状态被旧失败覆盖：%+v", state)
	}
}

func TestExitTechnicalsRejectMixedHistory(t *testing.T) {
	now := time.Now()
	generated := wideGenBars(wideGenDates(80, now.AddDate(0, 0, -1)), 10)
	bars := make([]model.DailyBar, 0, len(generated))
	for i, b := range generated {
		source := "eastmoney"
		if i == 30 {
			source = "sina"
		}
		bars = append(bars, model.DailyBar{Market: "cn", Symbol: "600094", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Source: source})
	}
	input := positionExitInput{position: model.Position{Market: "cn", Symbol: "600094", BuyPrice: 10, PeakPrice: 10},
		quote:   FreshQuoteResult{Quote: &datasource.Quote{Price: 10, High: 10, Low: 10, DataTime: now}, Fresh: quoteFreshInfo{Status: freshStatusFresh}},
		barRows: bars, session: model.PositionExitSessionIntraday, now: now}
	row := evaluatePositionExit(input, defaultPositionExitParams)
	if row.MA20 != 0 || row.MA60 != 0 || row.Trend != "unknown" || row.DataStatus == model.PositionExitDataReady {
		t.Fatalf("混源日线不能生成可用的卖出技术指标：%+v", row)
	}
}
