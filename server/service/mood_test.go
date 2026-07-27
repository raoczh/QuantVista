package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// cleanMoodTables 内存库 cache=shared 测试间共享，M3a 表为市场级公共数据，先清场。
func cleanMoodTables(t *testing.T) {
	t.Helper()
	for _, m := range []any{&model.LhbEntry{}, &model.LhbOrgDaily{}, &model.PopularityRank{},
		&model.LimitUpStock{}, &model.MarketMoodDaily{}, &model.FundFlowDaily{}} {
		common.DB.Where("1 = 1").Delete(m)
	}
	t.Cleanup(func() {
		for _, m := range []any{&model.LhbEntry{}, &model.LhbOrgDaily{}, &model.PopularityRank{},
			&model.LimitUpStock{}, &model.MarketMoodDaily{}, &model.FundFlowDaily{}} {
			common.DB.Where("1 = 1").Delete(m)
		}
	})
}

// computeMoodDaily：连板分布/最高连板/炸板率/昨涨停溢价/封板资金 top 的手工验算。
func TestComputeMoodDaily(t *testing.T) {
	zt := []datasource.ZTPoolItem{
		{Symbol: "000001", Streak: 1, SealFund: 1e8},
		{Symbol: "000002", Streak: 1, SealFund: 3e8},
		{Symbol: "000003", Streak: 2, SealFund: 2e8},
		{Symbol: "000004", Streak: 5, SealFund: 5e8},
		{Symbol: "000005", Streak: 0, SealFund: 0}, // 上游 lbc=0 兜底按 1 板计
	}
	yzt := []datasource.YZTPoolItem{
		{Symbol: "100001", ChangePct: 5},
		{Symbol: "100002", ChangePct: -3},
		{Symbol: "100003", ChangePct: 4},
	}
	m := computeMoodDaily("cn", "2026-07-08", zt, 3, yzt)
	if m.LimitUpCount != 5 || m.BrokenCount != 3 {
		t.Errorf("家数错误: %+v", m)
	}
	if m.BrokenRate != 37.5 { // 3/(5+3)*100
		t.Errorf("炸板率应 37.5，got %v", m.BrokenRate)
	}
	if m.MaxStreak != 5 || m.SealFundTop != 5e8 {
		t.Errorf("连板高度/封板资金错误: %+v", m)
	}
	var dist map[string]int
	if err := json.Unmarshal([]byte(m.StreakDistJSON), &dist); err != nil {
		t.Fatal(err)
	}
	if dist["1"] != 3 || dist["2"] != 1 || dist["5"] != 1 {
		t.Errorf("连板分布错误: %v", dist)
	}
	// 昨涨停溢价：(5-3+4)/3=2，红盘 2/3=66.67。
	if m.YztCount != 3 || m.YztAvgChg != 2 || m.YztUpRatio != 66.67 {
		t.Errorf("昨涨停溢价错误: %+v", m)
	}
	// 空池：不 panic、字段归零。
	empty := computeMoodDaily("cn", "2026-07-08", nil, 0, nil)
	if empty.BrokenRate != 0 || empty.StreakDistJSON != "" || empty.YztCount != 0 {
		t.Errorf("空池聚合应全零: %+v", empty)
	}
}

// SyncZTPools 端到端（注入 fetch 按 URL 分发三池假响应）+ 先删后插幂等。
func TestSyncZTPools(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	svc := NewMoodService()
	svc.em.SetFetchForTest(func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		switch {
		case strings.Contains(url, "getTopicZTPool"):
			return []byte(`{"rc":0,"data":{"tc":2,"qdate":20260708,"pool":[
				{"c":"002841","n":"视源股份","p":43080,"zdp":10.01,"amount":238093079,"ltsz":22459561813,"hs":1.06,"lbc":1,"fbt":92500,"lbt":92500,"fund":520606032,"zbc":0,"hybk":"消费电子","zttj":{"days":1,"ct":1}},
				{"c":"002497","n":"雅化集团","p":24650,"zdp":9.99,"amount":437457072,"ltsz":26118834709,"hs":1.67,"lbc":3,"fbt":92500,"lbt":93000,"fund":370881435,"zbc":1,"hybk":"化学制品","zttj":{"days":5,"ct":3}}]}}`), 200, nil
		case strings.Contains(url, "getTopicZBPool"):
			return []byte(`{"rc":0,"data":{"tc":1,"qdate":20260708,"pool":[{"c":"000938","n":"紫光股份","p":33620,"zdp":6.79,"hs":16.77,"zbc":2,"hybk":"IT服务"}]}}`), 200, nil
		case strings.Contains(url, "getYesterdayZTPool"):
			return []byte(`{"rc":0,"data":{"tc":2,"qdate":20260708,"pool":[
				{"c":"000973","n":"佛塑科技","zdp":-9.94,"ylbc":1,"hs":11.58,"hybk":"塑料"},
				{"c":"001206","n":"依依股份","zdp":5.57,"ylbc":1,"hs":15.48,"hybk":"个护用品"}]}}`), 200, nil
		}
		return []byte(`{"rc":102,"data":null}`), 200, nil
	})
	if err := svc.SyncZTPools(context.Background(), "2026-07-08"); err != nil {
		t.Fatal(err)
	}
	var cnt int64
	common.DB.Model(&model.LimitUpStock{}).Where("trade_date = ?", "2026-07-08").Count(&cnt)
	if cnt != 2 {
		t.Fatalf("涨停明细应 2 行，got %d", cnt)
	}
	var mood model.MarketMoodDaily
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", "2026-07-08").First(&mood).Error; err != nil {
		t.Fatal(err)
	}
	if mood.LimitUpCount != 2 || mood.BrokenCount != 1 || mood.MaxStreak != 3 {
		t.Errorf("聚合错误: %+v", mood)
	}
	if mood.BrokenRate != round2(1.0/3*100) {
		t.Errorf("炸板率错误: %v", mood.BrokenRate)
	}
	// 幂等：重跑先删后插，行数与聚合不翻倍。
	if err := svc.SyncZTPools(context.Background(), "2026-07-08"); err != nil {
		t.Fatal(err)
	}
	common.DB.Model(&model.LimitUpStock{}).Where("trade_date = ?", "2026-07-08").Count(&cnt)
	if cnt != 2 {
		t.Errorf("重跑后明细应仍 2 行，got %d", cnt)
	}
	var moodCnt int64
	common.DB.Model(&model.MarketMoodDaily{}).Count(&moodCnt)
	if moodCnt != 1 {
		t.Errorf("聚合应仍 1 行，got %d", moodCnt)
	}
	// moodBrief 消费口径。
	brief := moodBrief()
	if brief == nil || brief["max_streak"] != 3 || brief["trade_date"] != "2026-07-08" {
		t.Errorf("moodBrief 错误: %v", brief)
	}
}

// 龙虎榜 upsert 幂等 + 信号查询 + 详情页记录合并（上游解析已在 datasource 层锁定，
// 此处直测 upsert——datacenter 走包级 doGet 无法经 SetFetchForTest 注入）。
func TestSyncLhbAndSignals(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	// 信号水位 fail-closed 后（openDaysBehind 区间无日历记录返回 -1），seed 固定旧日期
	// 的数据必须钉日历与之齐平，否则 lhbSignalsFor 按「时效无法判定」正确拒绝。
	pinCalendarTo(t, "2026-07-07")
	svc := NewMoodService()
	lhbRows := []datasource.LhbRow{
		{Symbol: "002185", Name: "华天科技", TradeDate: "2026-07-07", ChangeType: "137001",
			Reason: "日涨幅偏离值达到7%", Note: "1家机构买入", Close: 21.93, ChangePct: 9.97,
			NetBuy: 1576984146, BuyAmt: 2255004980, SellAmt: 678020834, DealAmt: 2933025814,
			NetRatio: 13.14, TurnoverRate: 16.93},
		{Symbol: "002185", Name: "华天科技", TradeDate: "2026-07-07", ChangeType: "137016",
			Reason: "连续三日涨幅偏离累计20%", Close: 21.93, ChangePct: 9.97,
			NetBuy: 900000000, DealAmt: 2000000000},
	}
	orgRows := []datasource.LhbOrgRow{
		{Symbol: "002185", Name: "华天科技", TradeDate: "2026-07-07", Close: 21.93, ChangePct: 9.97,
			BuyTimes: 2, SellTimes: 0, BuyAmt: 3e8, NetBuy: 3e8, NetRatio: 2.5, Reason: "日涨幅偏离值达到7%"},
	}
	n, err := upsertLhbRows(lhbRows)
	if err != nil || n != 2 {
		t.Fatalf("应落 2 行，got %d err %v", n, err)
	}
	if err := upsertLhbOrgRows(orgRows); err != nil {
		t.Fatal(err)
	}
	// upsert 幂等（同键重复写入不新增）。
	if _, err := upsertLhbRows(lhbRows); err != nil {
		t.Fatal(err)
	}
	var cnt int64
	common.DB.Model(&model.LhbEntry{}).Count(&cnt)
	if cnt != 2 {
		t.Errorf("重跑后应仍 2 行，got %d", cnt)
	}

	// lhbSignalsFor：同股多原因取净买额最大的一条 + 机构信号合并。
	sigs := lhbSignalsFor(context.Background(), []string{"002185", "600000"})
	sig, ok := sigs["002185"]
	if !ok {
		t.Fatal("002185 应有龙虎榜信号")
	}
	if sig.NetBuyYi != round2(1576984146.0/1e8) || sig.OrgNetYi != 3 || sig.OrgBuys != 2 {
		t.Errorf("信号错误: %+v", sig)
	}
	if _, ok := sigs["600000"]; ok {
		t.Error("未上榜标的不应有信号")
	}

	// 详情页记录：2 行 + 机构净买合并。
	recs := svc.StockLhbRecords(context.Background(), "002185", 10)
	if len(recs) != 2 {
		t.Fatalf("上榜记录应 2 行，got %d", len(recs))
	}
	if recs[0].OrgNetBuy != 3e8 || recs[1].OrgNetBuy != 3e8 {
		t.Errorf("机构净买合并错误: %+v", recs)
	}
}

// 人气榜落库 + popSignalsFor。
func TestPopularitySignals(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	pinCalendarTo(t, "2026-07-08") // 水位 fail-closed：seed 日期须与日历期望齐平
	rows := []model.PopularityRank{
		{Symbol: "000725", Market: "cn", TradeDate: "2026-07-08", Rank: 1, PrevRank: 4},
		{Symbol: "002185", Market: "cn", TradeDate: "2026-07-08", Rank: 4, PrevRank: -3, IsNew: true},
		{Symbol: "600584", Market: "cn", TradeDate: "2026-07-07", Rank: 9, PrevRank: 2}, // 旧日期不取
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	sigs := popSignalsFor(context.Background(), []string{"000725", "002185", "600584"})
	if len(sigs) != 2 {
		t.Fatalf("应只取最新交易日 2 行，got %d", len(sigs))
	}
	if !sigs["002185"].IsNew || sigs["000725"].Rank != 1 {
		t.Errorf("人气信号错误: %+v", sigs)
	}
}

func TestMoodOverviewAggregation(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	moods := []model.MarketMoodDaily{
		{Market: "cn", TradeDate: "2026-07-08", LimitUpCount: 30, BrokenCount: 10, BrokenRate: 25, MaxStreak: 2},
		{Market: "cn", TradeDate: "2026-07-09", LimitUpCount: 45, BrokenCount: 5, BrokenRate: 10, MaxStreak: 4,
			YztAvgChg: 2.5, YztUpRatio: 70, StreakDistJSON: `{"1":2,"2":1,"4":1}`},
	}
	if err := common.DB.Create(&moods).Error; err != nil {
		t.Fatal(err)
	}
	stocks := []model.LimitUpStock{
		{Symbol: "600001", Market: "cn", TradeDate: "2026-07-09", Name: "一号", Streak: 1, SealFund: 1e8},
		{Symbol: "600002", Market: "cn", TradeDate: "2026-07-09", Name: "二号", Streak: 4, SealFund: 2e8},
		{Symbol: "600003", Market: "cn", TradeDate: "2026-07-09", Name: "三号", Streak: 2, SealFund: 5e8},
		{Symbol: "600004", Market: "cn", TradeDate: "2026-07-09", Name: "四号", Streak: 1, SealFund: 3e8},
	}
	if err := common.DB.Create(&stocks).Error; err != nil {
		t.Fatal(err)
	}

	view, err := NewMoodService().MoodOverview(context.Background(), "cn", 2)
	if err != nil {
		t.Fatal(err)
	}
	if view.Latest == nil || view.Latest.TradeDate != "2026-07-09" || view.StreakDist["4"] != 1 {
		t.Fatalf("最近快照/分布错误: %+v", view)
	}
	if len(view.Trend) != 2 || view.Trend[0].TradeDate != "2026-07-08" || view.Trend[1].TradeDate != "2026-07-09" {
		t.Fatalf("趋势应按日期升序: %+v", view.Trend)
	}
	if len(view.StreakLadders) != 3 || view.StreakLadders[0].Streak != 4 || view.StreakLadders[2].Count != 2 {
		t.Fatalf("连板梯队错误: %+v", view.StreakLadders)
	}
	if len(view.SealFundTop) != 4 || view.SealFundTop[0].Symbol != "600003" {
		t.Fatalf("封板资金排序错误: %+v", view.SealFundTop)
	}
	if _, err := NewMoodService().MoodOverview(context.Background(), "us", 10); err == nil {
		t.Fatal("非 A 股盘面情绪应拒绝")
	}
}

func TestLhbAndPopularityDaily(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	rows := []model.LhbEntry{
		{Symbol: "600011", Market: "cn", TradeDate: "2026-07-08", ChangeType: "old", Name: "旧日", NetBuy: 9e8},
		{Symbol: "600012", Market: "cn", TradeDate: "2026-07-09", ChangeType: "a", Name: "甲", NetBuy: 2e8, Reason: "原因甲"},
		{Symbol: "600013", Market: "cn", TradeDate: "2026-07-09", ChangeType: "b", Name: "乙", NetBuy: 5e8, Reason: "原因乙"},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.LhbOrgDaily{
		Symbol: "600013", Market: "cn", TradeDate: "2026-07-09", Name: "乙",
		NetBuy: 1.5e8, BuyTimes: 3, SellTimes: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	lhb, err := NewMoodService().LhbDaily(context.Background(), "cn", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if lhb.TradeDate != "2026-07-09" || len(lhb.Items) != 2 || lhb.Items[0].Symbol != "600013" || lhb.Items[0].OrgBuyTimes != 3 {
		t.Fatalf("龙虎榜日期回退/排序/机构合并错误: %+v", lhb)
	}
	if _, err := NewMoodService().LhbDaily(context.Background(), "cn", "20260709", 10); err == nil {
		t.Fatal("非法日期应拒绝")
	}

	const popSymbol = "689901"
	common.DB.Where("market = ? AND symbol = ?", "cn", popSymbol).Delete(&model.Stock{})
	t.Cleanup(func() { common.DB.Where("market = ? AND symbol = ?", "cn", popSymbol).Delete(&model.Stock{}) })
	if err := common.DB.Create(&model.Stock{Symbol: popSymbol, Market: "cn", Name: "人气股"}).Error; err != nil {
		t.Fatal(err)
	}
	popRows := []model.PopularityRank{
		{Symbol: "600011", Market: "cn", TradeDate: "2026-07-08", Rank: 1, PrevRank: 2},
		{Symbol: popSymbol, Market: "cn", TradeDate: "2026-07-09", Rank: 2, PrevRank: -3},
		{Symbol: "689902", Market: "cn", TradeDate: "2026-07-09", Rank: 1, PrevRank: 5},
	}
	if err := common.DB.Create(&popRows).Error; err != nil {
		t.Fatal(err)
	}
	pop, err := NewMoodService().PopularityDaily(context.Background(), "cn", "")
	if err != nil {
		t.Fatal(err)
	}
	if pop.TradeDate != "2026-07-09" || len(pop.Items) != 2 || pop.Items[0].Rank != 1 || !pop.Items[1].IsNew || pop.Items[1].Name != "人气股" {
		t.Fatalf("人气榜日期回退/排序/名称/新上榜错误: %+v", pop)
	}
}

func TestMoodQueriesHonorCanceledContext(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := NewMoodService()
	tests := []struct {
		name string
		call func() error
	}{
		{"overview", func() error { _, err := svc.MoodOverview(ctx, "cn", 20); return err }},
		{"lhb", func() error { _, err := svc.LhbDaily(ctx, "cn", "", 50); return err }},
		{"popularity", func() error { _, err := svc.PopularityDaily(ctx, "cn", ""); return err }},
		{"stock_lhb", func() error {
			if rows := svc.StockLhbRecords(ctx, "002185", 10); len(rows) != 0 {
				return errors.New("取消后仍返回龙虎榜记录")
			}
			return ctx.Err()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil {
				t.Fatal("请求 context 已取消时查询不应继续执行")
			}
		})
	}
}

// moodTargetDate：cutoff 前取上一开市日、cutoff 后取当日（周内），周末取周五。
func TestMoodTargetDate(t *testing.T) {
	setupTestDB(t)
	// 内存库 cache=shared：其它测试 seed 的交易日历会改变回退分支，先清空
	//（无日历 → isTradingDayToday 回退周一~五、prevOpenTradeDate 回退往前找工作日）。
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.TradingCalendar{}) })
	sat := time.Date(2026, 7, 11, 10, 0, 0, 0, time.Local) // 周六
	if got := moodTargetDate(sat, moodPoolCutoffMin); got != "2026-07-10" {
		t.Errorf("周六应取周五，got %s", got)
	}
	wedEarly := time.Date(2026, 7, 8, 10, 0, 0, 0, time.Local) // 周三盘中
	if got := moodTargetDate(wedEarly, moodPoolCutoffMin); got != "2026-07-07" {
		t.Errorf("cutoff 前应取上一开市日，got %s", got)
	}
	wedLate := time.Date(2026, 7, 8, 17, 0, 0, 0, time.Local)
	if got := moodTargetDate(wedLate, moodPoolCutoffMin); got != "2026-07-08" {
		t.Errorf("cutoff 后应取当日，got %s", got)
	}
	mon := time.Date(2026, 7, 13, 9, 0, 0, 0, time.Local) // 周一早
	if got := moodTargetDate(mon, moodLhbCutoffMin); got != "2026-07-10" {
		t.Errorf("周一早应取上周五，got %s", got)
	}
}

// runMoodPools 双游标拆分：涨停池成功推进池游标、人气榜未跑时人气游标独立保持为空
// （早前共用一个游标，人气榜失败会被涨停池成功带过、当天不再重试）；池游标已达标不重采。
func TestRunMoodPoolsSplitCursors(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	common.DB.Where("`key` IN ?", []string{optMoodPoolDay, optMoodPopDay}).Delete(&model.Option{})
	t.Cleanup(func() {
		common.DB.Where("`key` IN ?", []string{optMoodPoolDay, optMoodPopDay}).Delete(&model.Option{})
	})

	svc := NewMoodService()
	ztCalls := 0
	svc.em.SetFetchForTest(func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		if strings.Contains(url, "getTopicZTPool") {
			ztCalls++
			return []byte(`{"rc":0,"data":{"tc":1,"qdate":20260708,"pool":[{"c":"002841","n":"视源股份","p":43080,"zdp":10.01,"amount":238093079,"ltsz":22459561813,"hs":1.06,"lbc":1,"fbt":92500,"lbt":92500,"fund":520606032,"zbc":0,"hybk":"消费电子","zttj":{"days":1,"ct":1}}]}}`), 200, nil
		}
		return []byte(`{"rc":102,"data":null}`), 200, nil // 炸板/昨涨停 ErrNoData（正常态）
	})

	target := "2026-07-08"
	// target != today（今传 07-09）→ 人气榜跳过；涨停池成功推进池游标。
	svc.runMoodPools(context.Background(), target, "2026-07-09")
	if optionValue(optMoodPoolDay) != target {
		t.Fatalf("涨停池成功应推进池游标, got %q", optionValue(optMoodPoolDay))
	}
	if optionValue(optMoodPopDay) != "" {
		t.Fatalf("人气榜未跑，人气游标应独立保持空（游标已拆分，不再被涨停池带过）, got %q", optionValue(optMoodPopDay))
	}
	if ztCalls != 1 {
		t.Fatalf("涨停池应采一次, got %d", ztCalls)
	}
	// 再跑同日：池游标已达 target，涨停池不再重采（各自游标独立判断）。
	svc.runMoodPools(context.Background(), target, "2026-07-09")
	if ztCalls != 1 {
		t.Fatalf("池游标已达标不应重采, got %d", ztCalls)
	}
}

func TestSyncLhbAtomicReplace(t *testing.T) {
	setupTestDB(t)
	const tradeDate = "2026-07-27"

	seedOld := func(t *testing.T) {
		t.Helper()
		cleanMoodTables(t)
		if err := common.DB.Create(&model.LhbEntry{
			Symbol: "600001", Market: "cn", TradeDate: tradeDate,
			ChangeType: "old", Name: "旧主榜",
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.LhbOrgDaily{
			Symbol: "600001", Market: "cn", TradeDate: tradeDate, Name: "旧机构榜",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertOld := func(t *testing.T) {
		t.Helper()
		var mainRows []model.LhbEntry
		var orgRows []model.LhbOrgDaily
		common.DB.Where("trade_date = ?", tradeDate).Find(&mainRows)
		common.DB.Where("trade_date = ?", tradeDate).Find(&orgRows)
		if len(mainRows) != 1 || mainRows[0].Name != "旧主榜" || len(orgRows) != 1 || orgRows[0].Name != "旧机构榜" {
			t.Fatalf("失败后两表都应保持旧快照: main=%+v org=%+v", mainRows, orgRows)
		}
	}
	newMain := []datasource.LhbRow{{Symbol: "600002", Name: "新主榜", ChangeType: "new"}}

	t.Run("机构拉取失败不写库", func(t *testing.T) {
		seedOld(t)
		svc := NewMoodService()
		svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
			return newMain, nil
		}
		svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
			return nil, errors.New("机构接口故障")
		}
		if _, err := svc.SyncLhb(context.Background(), tradeDate); err == nil {
			t.Fatal("机构拉取失败应使整次同步失败")
		}
		assertOld(t)
	})

	t.Run("机构当日尚未发布不写库", func(t *testing.T) {
		seedOld(t)
		svc := NewMoodService()
		svc.now = func() time.Time {
			return time.Date(2026, 7, 27, 19, 15, 0, 0, time.Local)
		}
		svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
			return newMain, nil
		}
		svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
			return nil, datasource.ErrLhbNotReady
		}
		if _, err := svc.SyncLhb(context.Background(), tradeDate); !errors.Is(err, datasource.ErrLhbNotReady) {
			t.Fatalf("当日机构榜未发布应使整次同步重试，got %v", err)
		}
		assertOld(t)
	})

	t.Run("机构插入失败回滚两表", func(t *testing.T) {
		seedOld(t)
		svc := NewMoodService()
		svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
			return newMain, nil
		}
		svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
			return []datasource.LhbOrgRow{
				{Symbol: "600002", Name: "新机构榜 A"},
				{Symbol: "600002", Name: "新机构榜 B"}, // 同日同股触发唯一键冲突
			}, nil
		}
		if _, err := svc.SyncLhb(context.Background(), tradeDate); err == nil {
			t.Fatal("机构插入失败应使整次事务回滚")
		}
		assertOld(t)
	})

	t.Run("机构空榜提交完整快照", func(t *testing.T) {
		seedOld(t)
		svc := NewMoodService()
		svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
			return newMain, nil
		}
		svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
			return nil, datasource.ErrNoData
		}
		n, err := svc.SyncLhb(context.Background(), tradeDate)
		if err != nil || n != 1 {
			t.Fatalf("机构空榜应允许主榜完整提交, n=%d err=%v", n, err)
		}
		var mainRows []model.LhbEntry
		var orgCount int64
		common.DB.Where("trade_date = ?", tradeDate).Find(&mainRows)
		common.DB.Model(&model.LhbOrgDaily{}).Where("trade_date = ?", tradeDate).Count(&orgCount)
		if len(mainRows) != 1 || mainRows[0].Name != "新主榜" || orgCount != 0 {
			t.Fatalf("机构空榜应替换主榜并清空旧机构榜: main=%+v orgCount=%d", mainRows, orgCount)
		}
	})

	t.Run("历史日仍无机构结果可最终确认为空榜", func(t *testing.T) {
		seedOld(t)
		svc := NewMoodService()
		svc.now = func() time.Time {
			return time.Date(2026, 7, 28, 0, 30, 0, 0, time.Local)
		}
		svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
			return newMain, nil
		}
		svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
			return nil, datasource.ErrLhbNotReady
		}
		n, err := svc.SyncLhb(context.Background(), tradeDate)
		if err != nil || n != 1 {
			t.Fatalf("历史日可把持续无结果收口为空榜, n=%d err=%v", n, err)
		}
		var orgCount int64
		common.DB.Model(&model.LhbOrgDaily{}).Where("trade_date = ?", tradeDate).Count(&orgCount)
		if orgCount != 0 {
			t.Fatalf("历史空榜应清掉旧机构数据, got %d", orgCount)
		}
	})
}

func TestLhbPendingDates(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.TradingCalendar{}) })
	openDates := map[string]bool{
		"2026-07-01": true, "2026-07-03": true, "2026-07-24": true,
		"2026-07-27": true, "2026-07-28": true, "2026-07-31": true,
	}
	calendar := []model.TradingCalendar{{Market: "cn", TradeDate: "2026-06-30", IsOpen: true}}
	for day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local); !day.After(time.Date(2026, 7, 31, 0, 0, 0, 0, time.Local)); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		calendar = append(calendar, model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: openDates[date]})
	}
	if err := common.DB.Select("Market", "TradeDate", "IsOpen").Create(&calendar).Error; err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lhbPendingDates("2026-07-24", "2026-07-28"), ","); got != "2026-07-27,2026-07-28" {
		t.Fatalf("非空游标应只返回 cursor 后的开市日并升序排列, got %s", got)
	}
	if got := strings.Join(lhbPendingDates("", "2026-07-31"), ","); got != "2026-07-01,2026-07-03,2026-07-24,2026-07-27,2026-07-28,2026-07-31" {
		t.Fatalf("空游标应保留目标日前 30 个自然日的回填边界, got %s", got)
	}

	// 日历局部缺行时逐日回退：已知 07-27 开市，缺失的周二 07-28 不能被当休市跳过。
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-27", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	got, complete := lhbPendingDatesWithCoverage("2026-07-24", "2026-07-28")
	if complete || strings.Join(got, ",") != "2026-07-27" {
		t.Fatalf("稀疏日历必须标 unknown 且不得越过缺失工作日, dates=%v complete=%v", got, complete)
	}
}

func TestRunMoodLhbStopsAtFirstGap(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	common.DB.Where("`key` = ?", optMoodLhbDay).Delete(&model.Option{})
	t.Cleanup(func() {
		common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
		common.DB.Where("`key` = ?", optMoodLhbDay).Delete(&model.Option{})
	})
	calendar := []model.TradingCalendar{
		{Market: "cn", TradeDate: "2026-07-24", IsOpen: true},
		{Market: "cn", TradeDate: "2026-07-27", IsOpen: true},
		{Market: "cn", TradeDate: "2026-07-28", IsOpen: true},
	}
	if err := common.DB.Select("Market", "TradeDate", "IsOpen").Create(&calendar).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.UpsertOption(optMoodLhbDay, "2026-07-24"); err != nil {
		t.Fatal(err)
	}

	svc := NewMoodService()
	failMonday := true
	calls := []string{}
	svc.fetchLhbDaily = func(_ context.Context, date string) ([]datasource.LhbRow, error) {
		calls = append(calls, date)
		if date == "2026-07-27" && failMonday {
			return nil, errors.New("周一临时失败")
		}
		symbol := "600027"
		if date == "2026-07-28" {
			symbol = "600028"
		}
		return []datasource.LhbRow{{Symbol: symbol, Name: date, ChangeType: "daily"}}, nil
	}
	svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
		return nil, datasource.ErrNoData
	}

	if svc.runMoodLhb(context.Background(), "2026-07-28") {
		t.Fatal("周一失败时不应宣称已追平周二")
	}
	if got := strings.Join(calls, ","); got != "2026-07-27" {
		t.Fatalf("任一天失败后必须停止，不能越过缺口采周二, calls=%s", got)
	}
	if got := optionValue(optMoodLhbDay); got != "2026-07-24" {
		t.Fatalf("失败日不得推进游标, got %s", got)
	}

	failMonday = false
	calls = nil
	if !svc.runMoodLhb(context.Background(), "2026-07-28") {
		t.Fatal("下一轮应从周一开始并追平周二")
	}
	if got := strings.Join(calls, ","); got != "2026-07-27,2026-07-28" {
		t.Fatalf("周二补跑必须先补周一再采周二, calls=%s", got)
	}
	if got := optionValue(optMoodLhbDay); got != "2026-07-28" {
		t.Fatalf("补齐后游标应到周二, got %s", got)
	}
	var count int64
	common.DB.Model(&model.LhbEntry{}).Where("trade_date IN ?", []string{"2026-07-27", "2026-07-28"}).Count(&count)
	if count != 2 {
		t.Fatalf("周一、周二主榜都应落库, got %d", count)
	}
}

func TestRunMoodLhbRepairsIncompleteCalendarBeforeSync(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	common.DB.Where("`key` = ?", optMoodLhbDay).Delete(&model.Option{})
	t.Cleanup(func() {
		common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
		common.DB.Where("`key` = ?", optMoodLhbDay).Delete(&model.Option{})
	})
	if err := model.UpsertOption(optMoodLhbDay, "2026-07-24"); err != nil {
		t.Fatal(err)
	}
	// 只有周一一行，周二缺失；同步前必须先修复日历，不能越过 unknown。
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-27", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}

	svc := NewMoodService()
	repaired := 0
	svc.repairCalendar = func(context.Context) error {
		repaired++
		return common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-07-28", IsOpen: true}).Error
	}
	calls := []string{}
	svc.fetchLhbDaily = func(_ context.Context, date string) ([]datasource.LhbRow, error) {
		calls = append(calls, date)
		return []datasource.LhbRow{{Symbol: "600000", Name: date, ChangeType: "daily"}}, nil
	}
	svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
		return nil, datasource.ErrNoData
	}
	if !svc.runMoodLhb(context.Background(), "2026-07-28") {
		t.Fatal("日历修复后应按序追平")
	}
	if repaired != 1 || strings.Join(calls, ",") != "2026-07-27,2026-07-28" {
		t.Fatalf("应先修日历再连续同步: repaired=%d calls=%v", repaired, calls)
	}
}

func TestRunMoodLhbV2CursorIgnoresLegacyCompletion(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
	common.DB.Where("`key` IN ?", []string{optMoodLhbLegacyDay, optMoodLhbDay}).Delete(&model.Option{})
	t.Cleanup(func() {
		common.DB.Where("1 = 1").Delete(&model.TradingCalendar{})
		common.DB.Where("`key` IN ?", []string{optMoodLhbLegacyDay, optMoodLhbDay}).Delete(&model.Option{})
	})
	const target = "2026-07-28"
	if err := model.UpsertOption(optMoodLhbLegacyDay, target); err != nil {
		t.Fatal(err)
	}
	// 建完整窗口日历，仅 target 开市，让本测试只关注旧游标不能短路 v2 重验。
	start := time.Date(2026, 6, 28, 0, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 28, 0, 0, 0, 0, time.Local)
	calendar := make([]model.TradingCalendar, 0, 31)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		calendar = append(calendar, model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: date == target})
	}
	if err := common.DB.Select("Market", "TradeDate", "IsOpen").Create(&calendar).Error; err != nil {
		t.Fatal(err)
	}

	svc := NewMoodService()
	calls := 0
	svc.fetchLhbDaily = func(context.Context, string) ([]datasource.LhbRow, error) {
		calls++
		return []datasource.LhbRow{{Symbol: "600000", Name: "v2重验", ChangeType: "daily"}}, nil
	}
	svc.fetchLhbOrgDaily = func(context.Context, string) ([]datasource.LhbOrgRow, error) {
		return nil, datasource.ErrNoData
	}
	if !svc.runMoodLhb(context.Background(), target) || calls != 1 {
		t.Fatalf("旧完成游标不得短路 v2 重验: calls=%d v2=%q", calls, optionValue(optMoodLhbDay))
	}
}

func TestPopularityDailyNamePriority(t *testing.T) {
	setupTestDB(t)
	cleanMoodTables(t)
	symbols := []string{"689911", "689912", "689913", "689914"}
	for _, table := range []any{&model.StockUniverseDaily{}, &model.MarketSyncState{}, &model.Stock{}} {
		common.DB.Where("symbol IN ?", symbols).Delete(table)
	}
	t.Cleanup(func() {
		for _, table := range []any{&model.StockUniverseDaily{}, &model.MarketSyncState{}, &model.Stock{}} {
			common.DB.Where("symbol IN ?", symbols).Delete(table)
		}
	})
	stocks := []model.Stock{
		{Symbol: symbols[0], Market: "cn", Name: "基础名一"},
		{Symbol: symbols[1], Market: "cn", Name: "基础名二"},
		{Symbol: symbols[2], Market: "cn", Name: "基础名三"},
		{Symbol: symbols[3], Market: "cn", Name: "基础名四"},
	}
	states := []model.MarketSyncState{
		{Symbol: symbols[0], Market: "cn", Name: "状态名一"},
		{Symbol: symbols[1], Market: "cn", Name: "状态名二"},
		{Symbol: symbols[3], Market: "cn", Name: "状态名四"},
	}
	universe := []model.StockUniverseDaily{
		{TradeDate: "2999-12-30", Symbol: symbols[0], Market: "cn", Name: "宇宙旧名"},
		{TradeDate: "2999-12-31", Symbol: symbols[0], Market: "cn", Name: "宇宙最新名"},
		{TradeDate: "2999-12-31", Symbol: symbols[3], Market: "cn", Name: ""},
	}
	if err := common.DB.Create(&stocks).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&states).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&universe).Error; err != nil {
		t.Fatal(err)
	}
	for i, symbol := range symbols {
		if err := common.DB.Create(&model.PopularityRank{
			Symbol: symbol, Market: "cn", TradeDate: "2026-07-28", Rank: i + 1, PrevRank: i + 2,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	view, err := NewMoodService().PopularityDaily(context.Background(), "cn", "2026-07-28")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, item := range view.Items {
		names[item.Symbol] = item.Name
	}
	if names[symbols[0]] != "宇宙最新名" || names[symbols[1]] != "状态名二" || names[symbols[2]] != "基础名三" {
		t.Fatalf("名称来源优先级错误: %+v", names)
	}
	if names[symbols[3]] != "状态名四" {
		t.Fatalf("高优先级空名称不应遮蔽下一级来源: %+v", names)
	}
}

// TestPopularityDailyOrderSQLDialectSafe 人气榜排序不得使用裸 `ORDER BY rank`。
//
// rank 是 MySQL 8.0.2+ 保留字（窗口函数 RANK()），GORM 的 Order(string) 走
// clause.Column{Raw:true} 原样拼接不加引号 → 生产 MySQL 报 ERROR 1064，而 SQLite
// 不把 rank 当保留字，纯功能测试永远绿（这个 bug 就是这么漏过去的）。
// 两道断言：①源码里不得出现裸 Order("rank；②OrderByColumn 确实产出带引号的列名。
func TestPopularityDailyOrderSQLDialectSafe(t *testing.T) {
	setupTestDB(t)

	src, err := os.ReadFile("mood.go")
	if err != nil {
		t.Fatalf("读取 mood.go 失败: %v", err)
	}
	if strings.Contains(string(src), `Order("rank`) {
		t.Fatal(`mood.go 出现裸 Order("rank...")：rank 是 MySQL 保留字，须用 clause.OrderByColumn 让方言加引号`)
	}

	var rows []model.PopularityRank
	stmt := common.DB.Session(&gorm.Session{DryRun: true}).
		Where("market = ? AND trade_date = ?", "cn", "2026-07-28").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "rank"}}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "symbol"}}).
		Find(&rows).Statement
	sql := stmt.SQL.String()
	if strings.Contains(sql, "ORDER BY rank") {
		t.Fatalf("rank 未被引号包裹，MySQL 上是语法错误: %s", sql)
	}
	if !strings.Contains(sql, "`rank`") && !strings.Contains(sql, `"rank"`) {
		t.Fatalf("rank 应被方言引号包裹: %s", sql)
	}
}
