package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
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
	sigs := lhbSignalsFor([]string{"002185", "600000"})
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
	recs := svc.StockLhbRecords("002185", 10)
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
	sigs := popSignalsFor([]string{"000725", "002185", "600584"})
	if len(sigs) != 2 {
		t.Fatalf("应只取最新交易日 2 行，got %d", len(sigs))
	}
	if !sigs["002185"].IsNew || sigs["000725"].Rank != 1 {
		t.Errorf("人气信号错误: %+v", sigs)
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
