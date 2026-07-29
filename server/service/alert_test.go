package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

type fakeAlertMarket struct {
	getQuote      func(context.Context, string, string) (*datasource.Quote, error)
	getFreshQuote func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error)
	getDailyBars  func(context.Context, string, string, int) ([]datasource.Bar, error)
	getValuation  func(context.Context, string, string) (*datasource.Valuation, error)
}

func (f *fakeAlertMarket) GetQuote(ctx context.Context, market, symbol string) (*datasource.Quote, error) {
	if f.getQuote != nil {
		return f.getQuote(ctx, market, symbol)
	}
	return nil, datasource.ErrNoData
}

func (f *fakeAlertMarket) GetFreshQuote(ctx context.Context, market, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
	if f.getFreshQuote != nil {
		return f.getFreshQuote(ctx, market, symbol)
	}
	return nil, quoteFreshInfo{Status: freshStatusStale}, datasource.ErrNoData
}

func (f *fakeAlertMarket) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	if f.getDailyBars != nil {
		return f.getDailyBars(ctx, market, symbol, limit)
	}
	return nil, datasource.ErrNoData
}

func (f *fakeAlertMarket) GetValuation(ctx context.Context, market, symbol string) (*datasource.Valuation, error) {
	if f.getValuation != nil {
		return f.getValuation(ctx, market, symbol)
	}
	return nil, datasource.ErrNoData
}

func (f *fakeAlertMarket) QuoteFreshnessOf(string, time.Time) quoteFreshInfo {
	return quoteFreshInfo{Status: freshStatusFresh}
}

// FreshQuotesFor 默认实现：逐只走 GetFreshQuote（测试规模小，无需并发）。
// 真实 MarketService 是并发批量版本，两者语义一致：仅取到的标的进 map。
func (f *fakeAlertMarket) FreshQuotesFor(ctx context.Context, refs []QuoteRef) map[string]FreshQuoteResult {
	out := make(map[string]FreshQuoteResult, len(refs))
	for _, ref := range refs {
		q, fi, err := f.GetFreshQuote(ctx, ref.Market, ref.Symbol)
		if err != nil || q == nil {
			continue
		}
		out[QuoteKey(ref.Market, ref.Symbol)] = FreshQuoteResult{Quote: q, Fresh: fi}
	}
	return out
}

type recordingAlertNotifier struct {
	userID      int64
	calls       atomic.Int32
	contextLive atomic.Bool
	hasDeadline atomic.Bool
	lockHeld    atomic.Bool
	lastMessage NotifyMessage
}

func (n *recordingAlertNotifier) SendMsgContext(ctx context.Context, _ int64, msg NotifyMessage) {
	n.calls.Add(1)
	n.contextLive.Store(ctx.Err() == nil)
	_, hasDeadline := ctx.Deadline()
	n.hasDeadline.Store(hasDeadline)
	mu := alertEvalLock(n.userID)
	if mu.TryLock() {
		mu.Unlock()
	} else {
		n.lockHeld.Store(true)
	}
	n.lastMessage = msg
}

func TestAlertIntervalAtBoundaries(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 7, 27, hour, minute, 0, 0, time.Local)
	}
	tests := []struct {
		name       string
		hour       int
		minute     int
		tradingDay bool
		want       time.Duration
	}{
		{"早盘前", 9, 24, true, alertIntervalIdle},
		{"早盘开始", 9, 25, true, alertIntervalTrading},
		{"午休开始后", 11, 36, true, alertIntervalIdle},
		{"午盘开始", 12, 55, true, alertIntervalTrading},
		{"收盘保护窗末", 15, 5, true, alertIntervalTrading},
		{"保护窗后", 15, 6, true, alertIntervalIdle},
		{"休市日盘中", 10, 0, false, alertIntervalIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := alertIntervalAt(at(tt.hour, tt.minute), tt.tradingDay); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
	if got := alertNextDelayAt(at(9, 24), true); got != time.Minute {
		t.Fatalf("窗口前应准点唤醒，got %v", got)
	}
	if got := alertNextDelayAt(at(12, 50), true); got != 5*time.Minute {
		t.Fatalf("午盘前应准点唤醒，got %v", got)
	}
	if got := alertEvaluationTimeoutAt(at(10, 0), true); got >= alertIntervalTrading {
		t.Fatalf("盘中单轮超时必须短于调度间隔，got %v", got)
	}
	if got := alertEvaluationTimeoutAt(at(16, 0), true); got != alertTimeoutIdle {
		t.Fatalf("非交易窗口应使用空闲超时，got %v", got)
	}
	nearOpen := time.Date(2026, 7, 27, 9, 24, 30, 0, time.Local)
	if got := alertEvaluationTimeoutAt(nearOpen, true); got != 25*time.Second {
		t.Fatalf("开盘前空闲轮应提前释放锁，got %v", got)
	}
	tooLate := time.Date(2026, 7, 27, 9, 24, 56, 0, time.Local)
	if got := alertEvaluationTimeoutAt(tooLate, true); got != 0 {
		t.Fatalf("离开盘不足 guard 时应跳过空闲轮，got %v", got)
	}
}

func TestRotateAlertUserIDs(t *testing.T) {
	ids := []int64{1, 2, 3, 4, 5}
	got := rotateAlertUserIDs(ids, 2)
	want := []int64{3, 4, 5, 1, 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("轮转结果=%v，want %v", got, want)
	}
	got = rotateAlertUserIDs(ids, 7)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("超过长度的游标应取模，got %v", got)
	}
}

func TestAlertRoundRotationEventuallyCoversEveryUser(t *testing.T) {
	for _, total := range []int{9, 16, 20, 25, 100} {
		t.Run(fmt.Sprintf("users_%d", total), func(t *testing.T) {
			ids := make([]int64, total)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			seen := make(map[int64]bool, total)
			cursor := 0
			// 最坏情况：每轮只有固定 worker 数的首批用户能在预算内开始。
			for round := 0; round < total; round++ {
				rotated := rotateAlertUserIDs(ids, cursor)
				take := alertMaxConcurrentUsr
				if take > total {
					take = total
				}
				for _, uid := range rotated[:take] {
					seen[uid] = true
				}
				cursor += alertMaxConcurrentUsr
			}
			if len(seen) != total {
				t.Fatalf("轮转后仅覆盖 %d/%d 个用户", len(seen), total)
			}
		})
	}
}

func TestEvaluateAlertUsersBoundedConcurrency(t *testing.T) {
	const total = 12
	const workers = 3
	started := make(chan struct{}, total)
	release := make(chan struct{})
	done := make(chan struct{})
	var current atomic.Int32
	var maximum atomic.Int32
	evaluate := func(ctx context.Context, uid int64) (int, error) {
		n := current.Add(1)
		defer current.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	ids := make([]int64, total)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		if got := evaluateAlertUsers(ctx, ids, workers, time.Second, evaluate); got != total {
			t.Errorf("实际派发=%d，want %d", got, total)
		}
		close(done)
	}()

	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("初始 worker 未按时启动")
		}
	}
	select {
	case <-started:
		t.Fatal("并发数超过 worker 上限")
	case <-time.After(30 * time.Millisecond):
	}
	for launched := workers; launched < total; launched++ {
		release <- struct{}{}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("释放 worker 后下一用户未启动")
		}
	}
	for i := 0; i < workers; i++ {
		release <- struct{}{}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("有界 worker 池未退出")
	}
	if got := maximum.Load(); got != workers {
		t.Fatalf("最大并发=%d，want %d", got, workers)
	}
}

func TestEvaluateAlertUsersDispatchCancelDoesNotCancelStartedUser(t *testing.T) {
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	userCanceled := make(chan error, 1)
	done := make(chan int, 1)

	evaluate := func(ctx context.Context, _ int64) (int, error) {
		close(started)
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			userCanceled <- ctx.Err()
			return 0, ctx.Err()
		}
	}
	go func() {
		done <- evaluateAlertUsers(dispatchCtx, []int64{1, 2}, 1, 300*time.Millisecond, evaluate)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("首个用户未开始")
	}
	cancelDispatch()
	select {
	case err := <-userCanceled:
		t.Fatalf("派发窗口取消不应传给已开始用户: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case dispatched := <-done:
		if dispatched != 1 {
			t.Fatalf("实际派发=%d，want 1", dispatched)
		}
	case <-time.After(time.Second):
		t.Fatal("worker 池未退出")
	}
}

func TestEvaluateAlertUsersDoesNotDispatchAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	dispatched := evaluateAlertUsers(ctx, []int64{1, 2, 3}, 3, time.Second,
		func(context.Context, int64) (int, error) {
			calls.Add(1)
			return 0, nil
		})
	if dispatched != 0 || calls.Load() != 0 {
		t.Fatalf("截止后仍发生派发: dispatched=%d calls=%d", dispatched, calls.Load())
	}
}

func TestEvaluateAlertUsersEachUserGetsFreshTimeout(t *testing.T) {
	const perUser = 150 * time.Millisecond
	var secondRemaining time.Duration
	evaluate := func(ctx context.Context, uid int64) (int, error) {
		if uid == 1 {
			time.Sleep(60 * time.Millisecond)
			return 0, nil
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			return 0, errors.New("单用户 context 缺少 deadline")
		}
		secondRemaining = time.Until(deadline)
		return 0, nil
	}
	dispatchCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if dispatched := evaluateAlertUsers(dispatchCtx, []int64{1, 2}, 1, perUser, evaluate); dispatched != 2 {
		t.Fatalf("实际派发=%d，want 2", dispatched)
	}
	if secondRemaining < 120*time.Millisecond {
		t.Fatalf("后启动用户没有拿到独立完整预算，剩余仅 %v", secondRemaining)
	}
}

func TestTryEvaluateUserMarketSkipsBusyUser(t *testing.T) {
	const uid int64 = 987654321
	mu := alertEvalLock(uid)
	if err := mu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer mu.Unlock()

	svc := &AlertService{}
	if _, err := svc.tryEvaluateUserMarket(context.Background(), uid); !errors.Is(err, errAlertEvaluationBusy) {
		t.Fatalf("用户评估占用时应直接跳过，got %v", err)
	}
}

func TestEvaluateUserMarketLockWaitIsCancelable(t *testing.T) {
	const uid int64 = 987654322
	mu := alertEvalLock(uid)
	if err := mu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	svc := &AlertService{}
	if _, err := svc.evaluateUserMarket(ctx, uid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等待用户锁应响应 context，got %v", err)
	}
}

func TestAlertRuleMutationLockWaitIsCancelable(t *testing.T) {
	const uid int64 = 987654323
	mu := alertEvalLock(uid)
	if err := mu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	svc := &AlertService{}
	if _, err := svc.SetStatus(ctx, uid, 1, model.AlertStatusPaused); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("规则变更等待用户锁应响应 context，got %v", err)
	}
}

// TestEvaluateAlert_Price 到价：gte 用当日 high、lte 用当日 low 判盘中触达。
func TestEvaluateAlert_Price(t *testing.T) {
	// gte：现价未到但当日最高触及 → 命中。
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if !hit || msg == "" {
		t.Fatalf("当日最高触及应命中: hit=%v", hit)
	}
	// lte：当日最低触及止损位 → 命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpLTE, Threshold: 9},
		alertEval{Price: 9.5, DayHigh: 9.8, DayLow: 8.9},
	)
	if !hit {
		t.Fatalf("当日最低触及应命中")
	}
	// 未触及。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 12},
		alertEval{Price: 10.5, DayHigh: 11.2, DayLow: 10.0},
	)
	if hit {
		t.Fatalf("未触及不应命中")
	}
}

// TestEvaluateAlert_PctChange 异动：涨跌幅阈值。
func TestEvaluateAlert_PctChange(t *testing.T) {
	hit, v, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{ChangePct: 6.3},
	)
	if !hit || v != 6.3 {
		t.Fatalf("涨幅达标应命中: %v %v", hit, v)
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5},
		alertEval{ChangePct: -6.1},
	)
	if !hit {
		t.Fatalf("跌幅达标应命中")
	}
}

// TestEvaluateAlert_MA 均线：现价 vs MAn 站上/跌破，数据不足不命中。
func TestEvaluateAlert_MA(t *testing.T) {
	closes := []float64{9, 10, 11} // MA3 = 10
	hit, _, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.5, Closes: closes},
	)
	if !hit || msg == "" {
		t.Fatalf("站上 MA3 应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 9.5, Closes: closes},
	)
	if !hit {
		t.Fatalf("跌破 MA3 应命中")
	}
	// 数据不足：closes 少于 period。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 5},
		alertEval{Price: 10.5, Closes: closes},
	)
	if hit {
		t.Fatalf("日线不足不应命中")
	}
}

// TestEvaluateAlert_Breakout 突破：创近 N 日新高/新低（用当日 high/low）。
func TestEvaluateAlert_Breakout(t *testing.T) {
	highs := []float64{10.5, 10.8, 11.0} // 近 3 日前高 11.0
	lows := []float64{9.5, 9.2, 9.0}     // 近 3 日前低 9.0
	hit, _, _ := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 11.3, DayHigh: 11.3, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新高应命中")
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpLTE, Period: 3},
		alertEval{Price: 8.8, DayLow: 8.8, Highs: highs, Lows: lows},
	)
	if !hit {
		t.Fatalf("创新低应命中")
	}
	// 未破前高。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 3},
		alertEval{Price: 10.9, DayHigh: 10.9, Highs: highs, Lows: lows},
	)
	if hit {
		t.Fatalf("未破前高不应命中")
	}
}

// TestEvaluateAlert_VolumeSurge 放量：当日量 vs 20 日均量倍数，数据不足不命中。
func TestEvaluateAlert_VolumeSurge(t *testing.T) {
	// 20 根均量 1000 手，当日 2500 手 = 2.5 倍。
	volumes := make([]int64, 20)
	for i := range volumes {
		volumes[i] = 1000
	}
	hit, v, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 2500, Volumes: volumes},
	)
	if !hit || v != 2.5 || msg == "" {
		t.Fatalf("放量 2.5 倍 ≥ 2 应命中: hit=%v v=%v", hit, v)
	}
	// 未达倍数。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 3},
		alertEval{DayVolume: 2500, Volumes: volumes},
	)
	if hit {
		t.Fatalf("2.5 倍 < 3 不应命中")
	}
	// lte：缩量。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpLTE, Threshold: 0.5},
		alertEval{DayVolume: 300, Volumes: volumes},
	)
	if !hit {
		t.Fatalf("缩量 0.3 倍 ≤ 0.5 应命中")
	}
	// 历史量不足 20 根 → 不命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 2500, Volumes: volumes[:10]},
	)
	if hit {
		t.Fatalf("均量数据不足不应命中")
	}
	// 当日量缺失 → 不命中。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2},
		alertEval{DayVolume: 0, Volumes: volumes},
	)
	if hit {
		t.Fatalf("当日量缺失不应命中")
	}
}

// TestEvaluateAlert_Amplitude 振幅：达到阈值命中，数据缺失（0）不命中。
func TestEvaluateAlert_Amplitude(t *testing.T) {
	hit, v, msg := evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{Amplitude: 6.8},
	)
	if !hit || v != 6.8 || msg == "" {
		t.Fatalf("振幅 6.8%% ≥ 5%% 应命中: hit=%v v=%v", hit, v)
	}
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 8},
		alertEval{Amplitude: 6.8},
	)
	if hit {
		t.Fatalf("6.8%% < 8%% 不应命中")
	}
	// lte：窄幅震荡。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpLTE, Threshold: 2},
		alertEval{Amplitude: 1.1},
	)
	if !hit {
		t.Fatalf("振幅 1.1%% ≤ 2%% 应命中")
	}
	// 数据缺失。
	hit, _, _ = evaluateAlert(
		model.AlertRule{Kind: model.AlertKindAmplitude, Op: model.AlertOpGTE, Threshold: 5},
		alertEval{Amplitude: 0},
	)
	if hit {
		t.Fatalf("振幅数据缺失不应命中")
	}
}

// TestAlertCRUDIsolation CRUD + 用户隔离（DB 集成）。
func TestAlertCRUDIsolation(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_rules")
	common.DB.Exec("DELETE FROM alert_events")
	svc := &AlertService{}

	// 直接落库（跳过 Create 的行情校验，避免网络依赖）。
	r1 := &model.AlertRule{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11, Status: model.AlertStatusActive}
	r2 := &model.AlertRule{UserID: 1, Symbol: "000001", Market: "cn", Name: "平安银行",
		Kind: model.AlertKindPctChange, Op: model.AlertOpLTE, Threshold: -5, Status: model.AlertStatusTriggered}
	r3 := &model.AlertRule{UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2, Status: model.AlertStatusActive}
	other := &model.AlertRule{UserID: 2, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 20, Status: model.AlertStatusActive}
	for _, r := range []*model.AlertRule{r1, r2, r3, other} {
		if err := common.DB.Create(r).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	// List 用户1 → 3 条（隔离），triggered 排在前。
	rows, err := svc.List(1, "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("用户1 应有 3 条，得到 %d", len(rows))
	}
	if rows[0].Status != model.AlertStatusTriggered {
		t.Fatalf("命中的规则应排在前，得到 %s", rows[0].Status)
	}

	// 跨用户 Update/Delete 隔离。
	if _, err := svc.Update(context.Background(), 2, r1.ID, AlertInput{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 1}); err == nil {
		t.Fatalf("跨用户 Update 应失败")
	}
	if err := svc.Delete(context.Background(), 2, r1.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// 暂停恢复：SetStatus。
	if _, err := svc.SetStatus(context.Background(), 1, r2.ID, model.AlertStatusActive); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	var got model.AlertRule
	common.DB.First(&got, r2.ID)
	if got.Status != model.AlertStatusActive || got.TriggerMsg != "" {
		t.Fatalf("恢复后应 active 且清除命中标记: %+v", got)
	}

	// 删除规则：其未读事件应转已忽略（退出待办、保留历史）。
	common.DB.Create(&model.AlertEvent{RuleID: r1.ID, UserID: 1, Symbol: "600000", Market: "cn",
		Kind: model.AlertKindPrice, Message: "触及", TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	if err := svc.Delete(context.Background(), 1, r1.ID); err != nil {
		t.Fatalf("本人删除失败: %v", err)
	}
	rows, _ = svc.List(1, "")
	if len(rows) != 2 {
		t.Fatalf("删除后应剩 2 条，得到 %d", len(rows))
	}
	var ev model.AlertEvent
	common.DB.Where("rule_id = ?", r1.ID).First(&ev)
	if ev.Status != model.AlertEventDismissed {
		t.Fatalf("删规则后其未读事件应转 dismissed，得到 %s", ev.Status)
	}
}

// TestAlertEvents 命中事件状态机：同日去重落库、未读筛选、已读/忽略、全部已读、用户隔离。
func TestAlertEvents(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_events")
	svc := &AlertService{}
	now := time.Now()
	today := now.In(time.Local).Format("2006-01-02")

	// 首次命中：事件与规则状态同事务落库。
	rule := model.AlertRule{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	res, err := persistAlertEvaluation(context.Background(), rule, 11.2, true,
		"当日最高 11.20 触及目标价 ≥ 11.00", today, now)
	if err != nil || !res.eventCreated {
		t.Fatalf("首次命中应落事件: res=%+v err=%v", res, err)
	}
	// 同日再命中从数据库重读 triggered_at，去重不落新事件。
	res, err = persistAlertEvaluation(context.Background(), rule, 11.2, true, "重复命中", today, now)
	if err != nil || res.eventCreated {
		t.Fatalf("同日重复命中不应再落事件: res=%+v err=%v", res, err)
	}
	// 昨日命中过：把规则快照回拨后，今天再命中应落。
	old := now.AddDate(0, 0, -1)
	if err := common.DB.Model(&model.AlertRule{}).Where("id = ?", rule.ID).Update("triggered_at", old).Error; err != nil {
		t.Fatal(err)
	}
	res, err = persistAlertEvaluation(context.Background(), rule, 11.2, true, "次日再命中", today, now)
	if err != nil || !res.eventCreated {
		t.Fatalf("跨日再命中应落事件: res=%+v err=%v", res, err)
	}
	var cnt int64
	common.DB.Model(&model.AlertEvent{}).Where("user_id = ?", 1).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("用户1 应有 2 条事件，得到 %d", cnt)
	}

	// 他人事件（隔离用）。
	common.DB.Create(&model.AlertEvent{RuleID: 201, UserID: 2, Symbol: "000001", Market: "cn",
		Kind: model.AlertKindPrice, Message: "他人命中", TriggeredAt: now, Status: model.AlertEventUnread})

	// TriggeredForUser 只回本人 unread。
	trig, err := svc.TriggeredForUser(1)
	if err != nil {
		t.Fatalf("TriggeredForUser 失败: %v", err)
	}
	if len(trig) != 2 {
		t.Fatalf("用户1 未读事件应为 2，得到 %d", len(trig))
	}

	// SetEventStatus：已读；跨用户拒绝。
	if _, err := svc.SetEventStatus(1, trig[0].ID, model.AlertEventRead); err != nil {
		t.Fatalf("标记已读失败: %v", err)
	}
	if _, err := svc.SetEventStatus(2, trig[1].ID, model.AlertEventRead); err == nil {
		t.Fatalf("跨用户标记应失败")
	}
	if _, err := svc.SetEventStatus(1, trig[1].ID, "bogus"); err == nil {
		t.Fatalf("非法状态应拒绝")
	}
	trig, _ = svc.TriggeredForUser(1)
	if len(trig) != 1 {
		t.Fatalf("已读一条后未读应剩 1，得到 %d", len(trig))
	}

	// ListEvents 状态过滤。
	reads, _ := svc.ListEvents(1, model.AlertEventRead, 0)
	if len(reads) != 1 {
		t.Fatalf("已读列表应 1 条，得到 %d", len(reads))
	}
	all, _ := svc.ListEvents(1, "", 0)
	if len(all) != 2 {
		t.Fatalf("全部列表应 2 条（隔离他人），得到 %d", len(all))
	}

	// MarkAllEventsRead。
	n, err := svc.MarkAllEventsRead(1)
	if err != nil || n != 1 {
		t.Fatalf("全部已读应影响 1 条: n=%d err=%v", n, err)
	}
	trig, _ = svc.TriggeredForUser(1)
	if len(trig) != 0 {
		t.Fatalf("全部已读后未读应为 0，得到 %d", len(trig))
	}
	// 用户2 不受影响。
	trig2, _ := svc.TriggeredForUser(2)
	if len(trig2) != 1 {
		t.Fatalf("用户2 未读应仍为 1，得到 %d", len(trig2))
	}
}

func TestPositionAlertEventLifecycleDefense(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_events")
	common.DB.Exec("DELETE FROM positions")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM alert_events")
		common.DB.Exec("DELETE FROM positions")
	})
	svc := &AlertService{}
	now := time.Now()

	closed := model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "已平仓",
		Status: model.PositionStatusClosed}
	heldSibling := model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "同代码在持",
		Status: model.PositionStatusHolding}
	deleted := model.Position{UserID: 1, Symbol: "000001", Market: "cn", Name: "待删除",
		Status: model.PositionStatusHolding}
	otherHeld := model.Position{UserID: 2, Symbol: "600519", Market: "cn", Name: "他人持仓",
		Status: model.PositionStatusHolding}
	for _, p := range []*model.Position{&closed, &heldSibling, &deleted, &otherHeld} {
		if err := common.DB.Create(p).Error; err != nil {
			t.Fatalf("创建持仓失败: %v", err)
		}
	}
	deletedID := deleted.ID
	if err := common.DB.Delete(&deleted).Error; err != nil {
		t.Fatalf("删除测试持仓失败: %v", err)
	}

	nextRuleID := int64(1)
	createEvent := func(positionID int64, kind, status, message string) model.AlertEvent {
		t.Helper()
		ev := model.AlertEvent{RuleID: nextRuleID, UserID: 1, Symbol: "600000", Market: "cn",
			Kind: kind, Message: message, PositionID: positionID, TriggeredAt: now, Status: status}
		nextRuleID++
		if err := common.DB.Create(&ev).Error; err != nil {
			t.Fatalf("创建提醒事件失败: %v", err)
		}
		return ev
	}

	closedUnread := createEvent(closed.ID, model.AlertKindPeakDrawdown, model.AlertEventUnread, "已平仓脏未读")
	deletedUnread := createEvent(deletedID, model.AlertKindCostDrawdown, model.AlertEventUnread, "已删除脏未读")
	otherUnread := createEvent(otherHeld.ID, model.AlertKindCostGain, model.AlertEventUnread, "错绑他人持仓")
	heldUnread := createEvent(heldSibling.ID, model.AlertKindPeakDrawdown, model.AlertEventUnread, "有效持仓提醒")
	normalUnread := createEvent(0, model.AlertKindPrice, model.AlertEventUnread, "普通未读")
	closedRead := createEvent(closed.ID, model.AlertKindPeakDrawdown, model.AlertEventRead, "已平仓历史")
	deletedDismissed := createEvent(deletedID, model.AlertKindCostDrawdown, model.AlertEventDismissed, "已删除历史")
	otherRead := createEvent(otherHeld.ID, model.AlertKindCostGain, model.AlertEventRead, "他人持仓历史")
	heldRead := createEvent(heldSibling.ID, model.AlertKindPeakDrawdown, model.AlertEventRead, "有效持仓历史")
	normalRead := createEvent(0, model.AlertKindPrice, model.AlertEventRead, "普通历史")

	assertEventIDs := func(label string, rows []model.AlertEvent, want ...int64) {
		t.Helper()
		got := make(map[int64]bool, len(rows))
		for _, row := range rows {
			got[row.ID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("%s 条数错误: got=%d want=%d rows=%+v", label, len(got), len(want), rows)
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("%s 缺少事件 %d: %+v", label, id, rows)
			}
		}
	}

	triggered, err := svc.TriggeredForUser(1)
	if err != nil {
		t.Fatalf("查询未读提醒失败: %v", err)
	}
	assertEventIDs("TriggeredForUser", triggered, heldUnread.ID, normalUnread.ID)
	unread, err := svc.ListEvents(1, model.AlertEventUnread, 100)
	if err != nil {
		t.Fatalf("查询未读历史失败: %v", err)
	}
	assertEventIDs("ListEvents(unread)", unread, heldUnread.ID, normalUnread.ID)

	reads, _ := svc.ListEvents(1, model.AlertEventRead, 100)
	assertEventIDs("ListEvents(read)", reads, closedRead.ID, otherRead.ID, heldRead.ID, normalRead.ID)
	dismissed, _ := svc.ListEvents(1, model.AlertEventDismissed, 100)
	assertEventIDs("ListEvents(dismissed)", dismissed, deletedDismissed.ID)
	all, _ := svc.ListEvents(1, "all", 100)
	assertEventIDs("ListEvents(all)", all,
		closedUnread.ID, deletedUnread.ID, otherUnread.ID, heldUnread.ID, normalUnread.ID,
		closedRead.ID, deletedDismissed.ID, otherRead.ID, heldRead.ID, normalRead.ID)

	for name, ev := range map[string]model.AlertEvent{
		"closed": closedRead, "deleted": deletedDismissed, "other_user": otherRead,
	} {
		if _, err := svc.SetEventStatus(1, ev.ID, model.AlertEventUnread); err == nil {
			t.Fatalf("%s 持仓事件不得恢复未读", name)
		}
		var stored model.AlertEvent
		if err := common.DB.First(&stored, ev.ID).Error; err != nil {
			t.Fatalf("读取 %s 事件失败: %v", name, err)
		}
		if stored.Status != ev.Status {
			t.Fatalf("%s 恢复失败后状态被改写: got=%s want=%s", name, stored.Status, ev.Status)
		}
	}
	if _, err := svc.SetEventStatus(1, heldRead.ID, model.AlertEventUnread); err != nil {
		t.Fatalf("本人有效持仓事件应可恢复未读: %v", err)
	}
	if _, err := svc.SetEventStatus(1, normalRead.ID, model.AlertEventUnread); err != nil {
		t.Fatalf("PositionID=0 的普通事件应可恢复未读: %v", err)
	}

	triggered, err = svc.TriggeredForUser(1)
	if err != nil {
		t.Fatalf("恢复后查询未读提醒失败: %v", err)
	}
	assertEventIDs("恢复后的 TriggeredForUser", triggered,
		heldUnread.ID, normalUnread.ID, heldRead.ID, normalRead.ID)
}

func TestPersistAlertEvaluationRollsBackEventWhenRuleUpdateFails(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM alert_events")
	rule := model.AlertRule{UserID: 41, Symbol: "600000", Market: "cn", Name: "事务测试",
		Kind: model.AlertKindPrice, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Exec(`CREATE TRIGGER alert_rule_update_fail
		BEFORE UPDATE ON alert_rules WHEN NEW.id = ` + fmt.Sprint(rule.ID) + `
		BEGIN SELECT RAISE(ABORT, 'injected update failure'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Exec("DROP TRIGGER IF EXISTS alert_rule_update_fail") })

	now := time.Now()
	today := now.In(time.Local).Format("2006-01-02")
	if _, err := persistAlertEvaluation(context.Background(), rule, 11.2, true, "事务失败", today, now); err == nil {
		t.Fatal("规则更新失败时事务应返回错误")
	}
	var count int64
	common.DB.Model(&model.AlertEvent{}).Where("rule_id = ?", rule.ID).Count(&count)
	if count != 0 {
		t.Fatalf("规则更新失败后事件必须回滚，got %d", count)
	}
	var stored model.AlertRule
	common.DB.First(&stored, rule.ID)
	if stored.TriggeredAt != nil || stored.TriggerMsg != "" {
		t.Fatalf("规则最近命中状态不应半写: %+v", stored)
	}
}

func TestAlertRuleMutationWaitsForEvaluationLock(t *testing.T) {
	setupTestDB(t)
	rule := model.AlertRule{UserID: 42, Symbol: "600000", Market: "cn", Name: "锁测试",
		Kind: model.AlertKindPrice, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	mu := alertEvalLock(rule.UserID)
	if err := mu.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	svc := &AlertService{}
	go func() {
		_, err := svc.SetStatus(context.Background(), rule.UserID, rule.ID, model.AlertStatusPaused)
		done <- err
	}()
	select {
	case err := <-done:
		mu.Unlock()
		t.Fatalf("规则变更不应越过在途评估锁: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("释放评估锁后规则变更未完成")
	}
	var stored model.AlertRule
	common.DB.First(&stored, rule.ID)
	if stored.Status != model.AlertStatusPaused {
		t.Fatalf("规则应已暂停，got %s", stored.Status)
	}
}

func TestAlertRuleCursorRotatesAfterStartedAttempts(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 870001
	common.DB.Where("user_id = ?", userID).Delete(&model.AlertEvent{})
	common.DB.Where("user_id = ?", userID).Delete(&model.AlertRule{})
	alertRuleEvalCursors.Delete(userID)
	t.Cleanup(func() { alertRuleEvalCursors.Delete(userID) })

	rules := make([]model.AlertRule, 5)
	for i := range rules {
		rules[i] = model.AlertRule{
			UserID: userID, Symbol: fmt.Sprintf("R%d", i+1), Market: "cn",
			Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 100,
			Status: model.AlertStatusActive,
		}
		if err := common.DB.Create(&rules[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]bool)
	wantFirst := []string{"R1", "R4", "R2"}
	wantCursor := []int64{rules[2].ID, rules[0].ID, rules[3].ID}
	for round := 0; round < 3; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		completed := make([]string, 0, 2)
		market := &fakeAlertMarket{}
		market.getFreshQuote = func(_ context.Context, _ string, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
			if len(completed) == 2 {
				cancel()
				return nil, quoteFreshInfo{Status: freshStatusStale}, context.Canceled
			}
			completed = append(completed, symbol)
			seen[symbol] = true
			return &datasource.Quote{Symbol: symbol, Market: "cn", Price: 10, High: 10, Low: 9, PrevClose: 9.5},
				quoteFreshInfo{Status: freshStatusFresh}, nil
		}
		svc := &AlertService{market: market}
		_, err := svc.evaluateUserMarket(ctx, userID)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("第 %d 轮应在第 3 条开始时取消，got %v", round+1, err)
		}
		if len(completed) != 2 || completed[0] != wantFirst[round] {
			t.Fatalf("第 %d 轮完成顺序=%v，首条 want %s", round+1, completed, wantFirst[round])
		}
		cursor, _ := alertRuleEvalCursors.Load(userID)
		if cursor != wantCursor[round] {
			t.Fatalf("第 %d 轮游标=%v，want %d；已开始但超时的规则也必须推进", round+1, cursor, wantCursor[round])
		}
	}
	if len(seen) != len(rules) {
		t.Fatalf("三轮后仅覆盖 %d/%d 条规则: %v", len(seen), len(rules), seen)
	}
}

func TestAlertNotificationFlushesOnceOnNormalAndTimeout(t *testing.T) {
	tests := []struct {
		name      string
		userID    int64
		timeout   bool
		wantError error
	}{
		{name: "正常返回", userID: 880001},
		{name: "后续规则超时", userID: 880002, timeout: true, wantError: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDB(t)
			common.DB.Where("user_id = ?", tt.userID).Delete(&model.AlertEvent{})
			common.DB.Where("user_id = ?", tt.userID).Delete(&model.AlertRule{})
			common.DB.Where("user_id = ?", tt.userID).Delete(&model.NotifyChannel{})
			common.DB.Where("user_id = ?", tt.userID).Delete(&model.UserPreference{})
			alertRuleEvalCursors.Delete(tt.userID)
			t.Cleanup(func() { alertRuleEvalCursors.Delete(tt.userID) })

			if err := common.DB.Create(&model.UserPreference{UserID: tt.userID, EnableNotify: true}).Error; err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Create(&model.NotifyChannel{
				UserID: tt.userID, Kind: model.NotifyKindWebhook, Name: "test", Enabled: true,
			}).Error; err != nil {
				t.Fatal(err)
			}
			rules := []model.AlertRule{{
				UserID: tt.userID, Symbol: "HIT", Market: "cn", Name: "命中规则",
				Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 11,
				Status: model.AlertStatusActive,
			}}
			if tt.timeout {
				rules = append(rules, model.AlertRule{
					UserID: tt.userID, Symbol: "SLOW", Market: "cn", Name: "慢规则",
					Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 99,
					Status: model.AlertStatusActive,
				})
			}
			for i := range rules {
				if err := common.DB.Create(&rules[i]).Error; err != nil {
					t.Fatal(err)
				}
			}

			market := &fakeAlertMarket{}
			market.getFreshQuote = func(ctx context.Context, _ string, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
				if symbol == "SLOW" {
					<-ctx.Done()
					return nil, quoteFreshInfo{Status: freshStatusStale}, ctx.Err()
				}
				return &datasource.Quote{Symbol: symbol, Market: "cn", Price: 12, High: 12, Low: 10, PrevClose: 10},
					quoteFreshInfo{Status: freshStatusFresh}, nil
			}
			notifier := &recordingAlertNotifier{userID: tt.userID}
			svc := &AlertService{market: market, notify: notifier}
			var ctx context.Context = context.Background()
			var cancel context.CancelFunc = func() {}
			if tt.timeout {
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
			}
			hits, err := svc.evaluateUserMarket(ctx, tt.userID)
			cancel()
			if tt.wantError == nil && err != nil {
				t.Fatalf("评估失败: %v", err)
			}
			if tt.wantError != nil && !errors.Is(err, tt.wantError) {
				t.Fatalf("评估错误=%v，want %v", err, tt.wantError)
			}
			if hits != 1 {
				t.Fatalf("命中数=%d，want 1", hits)
			}
			if got := notifier.calls.Load(); got != 1 {
				t.Fatalf("推送尝试次数=%d，want 1", got)
			}
			if !notifier.contextLive.Load() || !notifier.hasDeadline.Load() {
				t.Fatalf("推送应使用独立且有界的有效 context: live=%v deadline=%v",
					notifier.contextLive.Load(), notifier.hasDeadline.Load())
			}
			if !notifier.lockHeld.Load() {
				t.Fatal("推送完成前必须继续持有用户评估锁")
			}
			if notifier.lastMessage.Content == "" {
				t.Fatal("推送内容不应为空")
			}
			var events int64
			common.DB.Model(&model.AlertEvent{}).Where("user_id = ?", tt.userID).Count(&events)
			if events != 1 {
				t.Fatalf("命中事件=%d，want 1", events)
			}
		})
	}
}
