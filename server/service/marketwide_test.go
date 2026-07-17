package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// ---------- 纯函数 ----------

func TestRelDiff(t *testing.T) {
	cases := []struct {
		a, b, want float64
	}{
		{10, 10, 0},
		{9, 10, 0.1},
		{10.05, 10, 0.005},
		{1, 0, 100}, // 分母下限 0.01
	}
	for _, c := range cases {
		got := relDiff(c.a, c.b)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("relDiff(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCloseMismatch(t *testing.T) {
	fresh := []datasource.Bar{
		{TradeDate: "2026-07-01", Close: 10.0},
		{TradeDate: "2026-07-02", Close: 10.2},
		{TradeDate: "2026-07-03", Close: 10.4},
	}
	// 完全一致：不判除权。
	db := map[string]float64{"2026-07-01": 10.0, "2026-07-02": 10.2, "2026-07-03": 10.4}
	if d, m := closeMismatch(db, fresh, rebaseTolerance); m {
		t.Fatalf("一致序列误判除权: %s", d)
	}
	// 容差内（0.3%）：不判。
	db["2026-07-02"] = 10.23
	if _, m := closeMismatch(db, fresh, rebaseTolerance); m {
		t.Fatal("容差内偏差误判除权")
	}
	// 任一点超容差（中间点，模拟半新半旧断层）：判除权。
	db["2026-07-01"] = 12.5
	if d, m := closeMismatch(db, fresh, rebaseTolerance); !m || d != "2026-07-01" {
		t.Fatalf("多点检测应命中 2026-07-01, got %s/%v", d, m)
	}
	// 无重叠日期：不判。
	if _, m := closeMismatch(map[string]float64{"2025-01-01": 5}, fresh, rebaseTolerance); m {
		t.Fatal("无重叠不应判除权")
	}
	// 0 值（缺失）不参与比对。
	if _, m := closeMismatch(map[string]float64{"2026-07-01": 0}, fresh, rebaseTolerance); m {
		t.Fatal("DB close=0 是缺失，不应参与比对")
	}
}

func TestSampleDates(t *testing.T) {
	dates := make([]string, 250)
	for i := range dates {
		dates[i] = fmt.Sprintf("d%03d", i)
	}
	got := sampleDates(dates, 60)
	if len(got) != 60 {
		t.Fatalf("采样数 = %d, want 60", len(got))
	}
	if got[0] != "d000" {
		t.Fatalf("采样必须覆盖头部（抓窗口外旧基准断层）: %s", got[0])
	}
	if got[len(got)-1] != "d249" {
		t.Fatalf("采样必须含末位: %s", got[len(got)-1])
	}
	// 少于 n 全保留。
	if got := sampleDates(dates[:10], 60); len(got) != 10 {
		t.Fatalf("不足 n 应全保留, got %d", len(got))
	}
}

func TestSpotTradeDate(t *testing.T) {
	ts := time.Date(2026, 7, 7, 15, 30, 0, 0, time.Local)
	rows := []datasource.SpotRow{
		{Symbol: "000001", DataTime: ts.Unix()},
		{Symbol: "000003", DataTime: ts.Add(-72 * time.Hour).Unix()}, // 停牌股旧时间戳
	}
	d, err := spotTradeDate(rows)
	if err != nil || d != "2026-07-07" {
		t.Fatalf("spotTradeDate = %s/%v, want 2026-07-07", d, err)
	}
	if _, err := spotTradeDate([]datasource.SpotRow{{Symbol: "000001"}}); err == nil {
		t.Fatal("f124 全缺失应报错（不猜日期）")
	}
}

// ---------- 集成（内存库 + 假数据源） ----------

// fakeWideSource 可编程的全市场链路假数据源。
type fakeWideSource struct {
	snapshot  []datasource.SpotRow
	snapErr   error
	bars      map[string][]datasource.Bar
	barsErr   map[string]error
	barsCalls map[string]int
	onBars    func(symbol string, nthCall int) // 副作用钩子（断点续传测试里触发 cancel）
}

func (f *fakeWideSource) GetCNSpotSnapshot(ctx context.Context) ([]datasource.SpotRow, error) {
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return f.snapshot, nil
}

func (f *fakeWideSource) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	if f.barsCalls == nil {
		f.barsCalls = map[string]int{}
	}
	f.barsCalls[symbol]++
	if f.onBars != nil {
		f.onBars(symbol, f.barsCalls[symbol])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err, ok := f.barsErr[symbol]; ok {
		return nil, err
	}
	bs, ok := f.bars[symbol]
	if !ok {
		return nil, datasource.ErrNoData
	}
	if limit > 0 && len(bs) > limit {
		bs = bs[len(bs)-limit:]
	}
	return bs, nil
}

// wideGenDates 以 end 为末位生成 n 个连续自然日（YYYY-MM-DD 升序）。
func wideGenDates(n int, end time.Time) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[n-1-i] = end.AddDate(0, 0, -i).Format("2006-01-02")
	}
	return out
}

// wideGenBars 按日期序列生成收盘恒为 close 的日线。
func wideGenBars(dates []string, close float64) []datasource.Bar {
	out := make([]datasource.Bar, 0, len(dates))
	for _, d := range dates {
		out = append(out, datasource.Bar{
			TradeDate: d, Open: close, High: close, Low: close, Close: close,
			Volume: 1000, Amount: close * 100000, TurnoverRate: 1.0,
		})
	}
	return out
}

// cleanWideTables 清场（内存库 cache=shared 测试间共享）。
func cleanWideTables(t *testing.T) {
	t.Helper()
	for _, m := range []any{&model.DailyBar{}, &model.MarketSyncState{}, &model.TradingCalendar{}, &model.DataSyncLog{}} {
		if err := common.DB.Where("1 = 1").Delete(m).Error; err != nil {
			t.Fatalf("清场失败: %v", err)
		}
	}
}

func barCount(t *testing.T, symbol string) int64 {
	t.Helper()
	var n int64
	common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND market = ?", symbol, "cn").Count(&n)
	return n
}

func stateOf(t *testing.T, symbol string) model.MarketSyncState {
	t.Helper()
	var st model.MarketSyncState
	if err := common.DB.Where("symbol = ? AND market = ?", symbol, "cn").First(&st).Error; err != nil {
		t.Fatalf("查 state %s 失败: %v", symbol, err)
	}
	return st
}

const wideTestTS = "2026-07-07 15:00:00"

func wideTestSnapshot() ([]datasource.SpotRow, string) {
	ts, _ := time.ParseInLocation("2006-01-02 15:04:05", wideTestTS, time.Local)
	rows := []datasource.SpotRow{
		{Symbol: "600001", Name: "甲股", Price: 10.5, Open: 10.2, High: 10.6, Low: 10.1, PrevClose: 10.0,
			Volume: 50000, Amount: 5.2e7, TurnoverRate: 2.5, DataTime: ts.Unix(), ChangePct: 5.0},
		{Symbol: "000100", Name: "乙股", Price: 5.05, Open: 5.0, High: 5.1, Low: 4.95, PrevClose: 5.0,
			Volume: 30000, Amount: 1.5e7, TurnoverRate: 1.2, DataTime: ts.Unix(), ChangePct: 1.0},
		{Symbol: "000003", Name: "停牌股", PrevClose: 2.71, DataTime: ts.Add(-72 * time.Hour).Unix()},
	}
	return rows, ts.Format("2006-01-02")
}

func TestSyncMarketWideEndToEnd(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	snap, tradeDate := wideTestSnapshot()
	svc := &MarketService{wide: &fakeWideSource{snapshot: snap}}

	log, err := svc.SyncMarketWide(context.Background())
	if err != nil {
		t.Fatalf("增量同步: %v", err)
	}
	if log.Total != 3 || log.Succeeded != 2 {
		t.Fatalf("log = total %d / bar %d, want 3/2（停牌股不落 bar）", log.Total, log.Succeeded)
	}
	// 当日 bar 落库口径。
	var bar model.DailyBar
	if err := common.DB.Where("symbol = ? AND trade_date = ?", "600001", tradeDate).First(&bar).Error; err != nil {
		t.Fatalf("当日 bar 未落库: %v", err)
	}
	if bar.Close != 10.5 || bar.TurnoverRate != 2.5 || bar.Source != "eastmoney" {
		t.Fatalf("bar 字段不符: %+v", bar)
	}
	// 宇宙字典：停牌股也在（pending），有效成交行带 last_bar_date。
	if st := stateOf(t, "000003"); st.InitStatus != "pending" || st.LastBarDate != "" {
		t.Fatalf("停牌股 state 不符: %+v", st)
	}
	if st := stateOf(t, "600001"); st.InitStatus != "pending" || st.LastBarDate != tradeDate {
		t.Fatalf("有效成交行 state 不符: %+v", st)
	}
	// 日历补当日。
	var cal model.TradingCalendar
	if err := common.DB.Where("market = ? AND trade_date = ?", "cn", tradeDate).First(&cal).Error; err != nil || !cal.IsOpen {
		t.Fatalf("日历未补当日: %v %+v", err, cal)
	}
	// S0-3 宇宙快照必须整批落库（回归锚：AssignmentColumns 列名与 GORM 蛇形化不符
	//（如 PETTM→pettm 写成 pe_ttm）会让整条 upsert SQL 无效、快照一行都落不了）。
	var uniN int64
	common.DB.Model(&model.StockUniverseDaily{}).Where("trade_date = ?", tradeDate).Count(&uniN)
	if uniN != 3 {
		t.Fatalf("宇宙快照应落 3 行（含停牌股），得到 %d", uniN)
	}

	// 幂等：重跑一遍 bars/states 不翻倍。
	if _, err := svc.SyncMarketWide(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if n := barCount(t, "600001"); n != 1 {
		t.Fatalf("重跑后 bar 数 = %d, want 1（幂等）", n)
	}
	var stN int64
	common.DB.Model(&model.MarketSyncState{}).Count(&stN)
	if stN != 3 {
		t.Fatalf("重跑后 states = %d, want 3", stN)
	}
	// 快照 upsert 幂等（重跑覆盖为最新，不翻倍）。
	common.DB.Model(&model.StockUniverseDaily{}).Where("trade_date = ?", tradeDate).Count(&uniN)
	if uniN != 3 {
		t.Fatalf("重跑后宇宙快照 = %d, want 3（upsert 幂等）", uniN)
	}
}

func TestSyncMarketWideRebaseSuspect(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	snap, tradeDate := wideTestSnapshot()
	end, _ := time.ParseInLocation("2006-01-02", tradeDate, time.Local)
	prevDate := end.AddDate(0, 0, -1).Format("2006-01-02")

	// 预埋：日历上一交易日 + 甲股旧基准历史（除权前 close=13.0，快照 f18 昨收=10.0 → 偏差 30%）。
	common.DB.Select("Market", "TradeDate", "IsOpen").Create(&model.TradingCalendar{Market: "cn", TradeDate: prevDate, IsOpen: true})
	oldDates := wideGenDates(120, end.AddDate(0, 0, -1))
	for _, b := range wideGenBars(oldDates, 13.0) {
		common.DB.Create(&model.DailyBar{Symbol: "600001", Market: "cn", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Amount: b.Amount, Source: "eastmoney"})
	}

	// 东财 kline 返回 250 根新基准（前复权整体重锚后的序列，末根=当日）。
	fake := &fakeWideSource{
		snapshot: snap,
		bars:     map[string][]datasource.Bar{"600001": wideGenBars(wideGenDates(250, end), 10.0)},
	}
	svc := &MarketService{wide: fake}
	log, err := svc.SyncMarketWide(context.Background())
	if err != nil {
		t.Fatalf("增量同步: %v", err)
	}
	// 甲股被整体重锚：全库恰 250 根、全部新基准，无旧基准残留（衔接无断层）。
	if n := barCount(t, "600001"); n != 250 {
		t.Fatalf("重锚后 bar 数 = %d, want 250", n)
	}
	var stale int64
	common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND close > ?", "600001", 10.01).Count(&stale)
	if stale != 0 {
		t.Fatalf("残留旧基准 %d 根（断层未清）", stale)
	}
	if st := stateOf(t, "600001"); st.AdjustEpoch == "" || st.InitStatus != "done" {
		t.Fatalf("重锚后 state 不符: %+v", st)
	}
	// 乙股无历史不受影响；日志记录了重锚。
	if log.Failed != 0 {
		t.Fatalf("重锚不应失败: %+v", log)
	}
}

func TestPersistDailyBarsDetectRebase(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	end := time.Date(2026, 7, 7, 0, 0, 0, 0, time.Local)
	allDates := wideGenDates(250, end)

	// DB 预埋 250 根旧基准 close=20；在线路径拉到尾部 90 根新基准 close=10（部分窗口）。
	for _, b := range wideGenBars(allDates, 20.0) {
		common.DB.Create(&model.DailyBar{Symbol: "600002", Market: "cn", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Source: "eastmoney"})
	}
	freshTail := wideGenBars(allDates[160:], 10.0)
	for i := range freshTail {
		freshTail[i].Source = "eastmoney"
	}
	fake := &fakeWideSource{bars: map[string][]datasource.Bar{"600002": wideGenBars(allDates, 10.0)}}
	svc := &MarketService{wide: fake}

	// 部分窗口落库 → 多点检测命中 → 自动全量重锚（防"部分窗口重写"漏检的核心场景）。
	svc.persistDailyBars("cn", "600002", freshTail)
	if n := barCount(t, "600002"); n != 250 {
		t.Fatalf("重锚后 bar 数 = %d, want 250", n)
	}
	var stale int64
	common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND close > ?", "600002", 10.01).Count(&stale)
	if stale != 0 {
		t.Fatalf("残留旧基准 %d 根", stale)
	}

	// 反例：新浪源（不复权）不触发检测——旧行为窗口 upsert（承认的混源边界）。
	cleanWideTables(t)
	for _, b := range wideGenBars(allDates[:120], 20.0) {
		common.DB.Create(&model.DailyBar{Symbol: "600003", Market: "cn", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Source: "eastmoney"})
	}
	sinaBars := wideGenBars(allDates[60:120], 10.0)
	for i := range sinaBars {
		sinaBars[i].Source = "sina"
	}
	svc.persistDailyBars("cn", "600003", sinaBars)
	if n := barCount(t, "600003"); n != 120 {
		t.Fatalf("sina 源不应触发重锚, bar 数 = %d, want 120", n)
	}
	var head model.DailyBar
	common.DB.Where("symbol = ? AND trade_date = ?", "600003", allDates[0]).First(&head)
	if head.Close != 20.0 {
		t.Fatalf("sina 源不应重写头部: %+v", head)
	}
}

func TestPersistDailyBarsRebaseFailFallback(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	end := time.Date(2026, 7, 7, 0, 0, 0, 0, time.Local)
	allDates := wideGenDates(200, end)

	// 预埋旧基准 + states 行（done）；重拉挂掉 → 检测命中但重锚失败 →
	// 退回旧行为写本窗，并把 states 踢回 pending 交给初始化任务自愈。
	for _, b := range wideGenBars(allDates, 20.0) {
		common.DB.Create(&model.DailyBar{Symbol: "600004", Market: "cn", TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Source: "eastmoney"})
	}
	common.DB.Create(&model.MarketSyncState{Symbol: "600004", Market: "cn", InitStatus: "done"})
	fake := &fakeWideSource{barsErr: map[string]error{"600004": errors.New("EOF")}}
	svc := &MarketService{wide: fake}

	fresh := wideGenBars(allDates[150:], 10.0)
	for i := range fresh {
		fresh[i].Source = "eastmoney"
	}
	svc.persistDailyBars("cn", "600004", fresh)
	// 本窗已写（尾部新基准），头部残留旧基准（断层），但 states 已标 pending。
	var tail model.DailyBar
	common.DB.Where("symbol = ? AND trade_date = ?", "600004", allDates[199]).First(&tail)
	if tail.Close != 10.0 {
		t.Fatalf("重锚失败应退回写本窗: %+v", tail)
	}
	if st := stateOf(t, "600004"); st.InitStatus != "pending" {
		t.Fatalf("重锚失败应踢回 pending 自愈: %+v", st)
	}
}

func TestInitMarketWideHistoryResumeAndFail(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	end := time.Date(2026, 7, 7, 0, 0, 0, 0, time.Local)
	dates := wideGenDates(250, end)
	for _, sym := range []string{"600011", "600012", "600013"} {
		common.DB.Create(&model.MarketSyncState{Symbol: sym, Market: "cn", Name: sym, InitStatus: "pending"})
	}
	fake := &fakeWideSource{
		bars: map[string][]datasource.Bar{
			"600011": wideGenBars(dates, 8.0),
			"600012": wideGenBars(dates, 9.0),
			"600013": wideGenBars(dates, 7.0),
		},
	}
	svc := &MarketService{wide: fake}

	// 第 1 只完成后取消 → 断点：1 done / 2 pending。
	ctx, cancel := context.WithCancel(context.Background())
	fake.onBars = func(symbol string, nth int) {
		if symbol == "600012" {
			cancel() // 第 2 只拉取时预算被掐
		}
	}
	if _, err := svc.initMarketWideHistory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context.Canceled, got %v", err)
	}
	if st := stateOf(t, "600011"); st.InitStatus != "done" || st.BarsCount != 250 {
		t.Fatalf("断点前标的应 done: %+v", st)
	}
	if st := stateOf(t, "600012"); st.InitStatus != "pending" || st.FailCount != 0 {
		t.Fatalf("取消不是标的问题，不应记失败: %+v", st)
	}

	// 续跑（无取消）：剩余全部 done，已 done 的不再重拉。
	fake.onBars = nil
	before := fake.barsCalls["600011"]
	log, err := svc.initMarketWideHistory(context.Background())
	if err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if log.Succeeded != 2 {
		t.Fatalf("续跑应只处理剩余 2 只, got %d", log.Succeeded)
	}
	if fake.barsCalls["600011"] != before {
		t.Fatal("已 done 标的不应重拉（断点续传）")
	}
	if n := barCount(t, "600013"); n != 250 {
		t.Fatalf("600013 bar 数 = %d, want 250", n)
	}

	// 失败标的：连续 wideInitMaxFail 轮后置 failed 不再骚扰。
	cleanWideTables(t)
	common.DB.Create(&model.MarketSyncState{Symbol: "600099", Market: "cn", InitStatus: "pending"})
	fake2 := &fakeWideSource{barsErr: map[string]error{"600099": datasource.ErrNoData}}
	svc2 := &MarketService{wide: fake2}
	for i := 0; i < wideInitMaxFail; i++ {
		if _, err := svc2.initMarketWideHistory(context.Background()); err != nil {
			t.Fatalf("第 %d 轮: %v", i+1, err)
		}
	}
	if st := stateOf(t, "600099"); st.InitStatus != "failed" || st.FailCount != wideInitMaxFail {
		t.Fatalf("连续失败应置 failed: %+v", st)
	}
	if _, err := svc2.initMarketWideHistory(context.Background()); err != nil {
		t.Fatalf("failed 后再跑: %v", err)
	}
	if fake2.barsCalls["600099"] != wideInitMaxFail {
		t.Fatalf("failed 标的不应再被拉取: %d", fake2.barsCalls["600099"])
	}
}

func TestInitMarketWideHistorySourceAbort(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	// 全部标的网络类失败（EOF，非 ErrNoData）→ 连续达阈值判源故障：
	// 本轮中止且不把失败记到标的头上（防限流期把全宇宙误标 failed）。
	n := wideInitAbortStreak + 5
	for i := 0; i < n; i++ {
		common.DB.Create(&model.MarketSyncState{Symbol: fmt.Sprintf("6001%02d", i), Market: "cn", InitStatus: "pending"})
	}
	fake := &fakeWideSource{} // bars 空 map 也会 ErrNoData——用 barsErr 全部给网络错
	fake.barsErr = map[string]error{}
	for i := 0; i < n; i++ {
		fake.barsErr[fmt.Sprintf("6001%02d", i)] = errors.New("EOF")
	}
	svc := &MarketService{wide: fake}
	log, err := svc.initMarketWideHistory(context.Background())
	if err != nil {
		t.Fatalf("源故障中止不应返回错误: %v", err)
	}
	if log.Status != "failed" || log.Total != 0 {
		t.Fatalf("中止日志不符: status=%s total=%d", log.Status, log.Total)
	}
	var touched int64
	common.DB.Model(&model.MarketSyncState{}).
		Where("init_status <> ? OR fail_count > 0", "pending").Count(&touched)
	if touched != 0 {
		t.Fatalf("源故障不应记到标的头上, 被动过的行 = %d", touched)
	}
	// 只探测了阈值只数就停手（不再空扫加速打上游）。
	if calls := len(fake.barsCalls); calls != wideInitAbortStreak {
		t.Fatalf("中止前拉取次数 = %d, want %d", calls, wideInitAbortStreak)
	}
}

func TestInitMarketWideHistoryScatteredNetFail(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	end := time.Date(2026, 7, 7, 0, 0, 0, 0, time.Local)
	dates := wideGenDates(250, end)
	// 成功之间夹杂的零散网络失败：源活着，失败记到标的头上（一次尝试）。
	for _, sym := range []string{"600021", "600022", "600023"} {
		common.DB.Create(&model.MarketSyncState{Symbol: sym, Market: "cn", InitStatus: "pending"})
	}
	fake := &fakeWideSource{
		bars: map[string][]datasource.Bar{
			"600021": wideGenBars(dates, 8.0),
			"600023": wideGenBars(dates, 7.0),
		},
		barsErr: map[string]error{"600022": errors.New("connection reset")},
	}
	svc := &MarketService{wide: fake}
	log, err := svc.initMarketWideHistory(context.Background())
	if err != nil {
		t.Fatalf("零散失败轮: %v", err)
	}
	if log.Succeeded != 2 || log.Failed != 1 {
		t.Fatalf("log = %d/%d, want 2 成功 1 失败", log.Succeeded, log.Failed)
	}
	if st := stateOf(t, "600022"); st.InitStatus != "pending" || st.FailCount != 1 {
		t.Fatalf("零散网络失败应记一次尝试并保持 pending: %+v", st)
	}
}

// 落库失败不标 done：拉取成功但 persistDailyBars 落库失败（daily_bars 表缺失模拟 DB 故障）
// 时，必须走 recordFail 保持 pending，不能标 done 留永久缺口。
func TestInitMarketWideHistoryPersistFailNotDone(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	end := time.Date(2026, 7, 7, 0, 0, 0, 0, time.Local)
	dates := wideGenDates(250, end)
	common.DB.Create(&model.MarketSyncState{Symbol: "600055", Market: "cn", InitStatus: "pending"})
	fake := &fakeWideSource{bars: map[string][]datasource.Bar{"600055": wideGenBars(dates, 8.0)}}
	svc := &MarketService{wide: fake}

	// 删掉 daily_bars 表 → persistDailyBars 的 upsert 必失败（返回 error）。
	if err := common.DB.Migrator().DropTable(&model.DailyBar{}); err != nil {
		t.Fatalf("drop daily_bars: %v", err)
	}
	t.Cleanup(func() { common.DB.AutoMigrate(&model.DailyBar{}) })

	log, err := svc.initMarketWideHistory(context.Background())
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if log.Succeeded != 0 || log.Failed != 1 {
		t.Fatalf("落库失败应记失败不记成功: log = %d/%d, want 0/1", log.Succeeded, log.Failed)
	}
	if st := stateOf(t, "600055"); st.InitStatus != "pending" || st.FailCount != 1 {
		t.Fatalf("落库失败不应标 done，应保持 pending 计一次尝试: %+v", st)
	}
}
