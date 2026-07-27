package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/datasource"
)

func TestBuildMinuteLineLatestDayAndAverage(t *testing.T) {
	bars := []datasource.Min5Bar{
		// OHLC 典型价为 16，与 close=12 明显不同，锚定计划要求的 close 加权口径。
		{Time: "202607270932", Open: 20, High: 21, Low: 11, Close: 12, Volume: 300},
		{Time: "202607241500", Open: 9, High: 9, Low: 9, Close: 9, Volume: 999},
		{Time: "202607270931", Open: 8, High: 12, Low: 7, Close: 10, Volume: 100},
	}
	line, err := buildMinuteLine("cn", "000001", bars, 9.8)
	if err != nil {
		t.Fatal(err)
	}
	if line.TradeDate != "2026-07-27" || len(line.Points) != 2 {
		t.Fatalf("应只取末根所属日: %+v", line)
	}
	if line.Points[0].Time != "09:31" || line.Points[1].Avg != 11.5 {
		t.Fatalf("时刻/累计估算均价错误: %+v", line.Points)
	}
	if line.PrevClose != 9.8 || line.TotalVolume != 400 || line.Last != 12 || line.High != 21 || line.Low != 7 {
		t.Fatalf("汇总字段错误: %+v", line)
	}
}

func TestBuildMinuteLinePrecFallback(t *testing.T) {
	line, err := buildMinuteLine("cn", "000002", []datasource.Min5Bar{{
		Time: "202607270931", Open: 10.123, High: 10.2, Low: 10.1, Close: 10.15, Volume: 0,
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !line.BaseFromOpen || line.PrevClose != 10.123 || line.Points[0].Avg != 10.15 {
		t.Fatalf("prec 缺失回退或零量均价错误: %+v", line)
	}
}

func resetMinuteLineTestState(t *testing.T) {
	t.Helper()
	minuteLineMu.Lock()
	minuteLineCache = map[string]minuteLineCacheEntry{}
	minuteLineFlights = map[string]*minuteLineFlight{}
	minuteLineMu.Unlock()
	t.Cleanup(func() {
		minuteLineMu.Lock()
		minuteLineCache = map[string]minuteLineCacheEntry{}
		minuteLineFlights = map[string]*minuteLineFlight{}
		minuteLineMu.Unlock()
	})
}

func TestMinuteLineCacheAndErrors(t *testing.T) {
	resetMinuteLineTestState(t)

	calls := 0
	svc := &IntradayService{fetchMin1: func(context.Context, string, string, int) ([]datasource.Min5Bar, float64, error) {
		calls++
		return []datasource.Min5Bar{{Time: "202607270931", Open: 10, High: 10, Low: 10, Close: 10, Volume: 1}}, 9.9, nil
	}}
	for i := 0; i < 2; i++ {
		if _, err := svc.MinuteLine(context.Background(), "cn", "600001"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("60 秒缓存内应只拉一次，got %d", calls)
	}
	if _, err := svc.MinuteLine(context.Background(), "us", "AAPL"); err == nil || !strings.Contains(err.Error(), "仅支持 A 股") {
		t.Fatalf("非 A 股应明确拒绝，got %v", err)
	}

	noData := &IntradayService{fetchMin1: func(context.Context, string, string, int) ([]datasource.Min5Bar, float64, error) {
		return nil, 0, datasource.ErrNoData
	}}
	if _, err := noData.MinuteLine(context.Background(), "cn", "600002"); err == nil || !strings.Contains(err.Error(), "暂无分时数据") {
		t.Fatalf("无数据错误边界不正确: %v", err)
	}
	upstream := errors.New("timeout")
	badSource := &IntradayService{fetchMin1: func(context.Context, string, string, int) ([]datasource.Min5Bar, float64, error) {
		return nil, 0, upstream
	}}
	if _, err := badSource.MinuteLine(context.Background(), "cn", "600003"); err == nil || !errors.Is(err, upstream) {
		t.Fatalf("上游错误应保留 cause: %v", err)
	}
}

func TestMinuteLineConcurrentMissCoalesced(t *testing.T) {
	resetMinuteLineTestState(t)

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	svc := &IntradayService{fetchMin1: func(context.Context, string, string, int) ([]datasource.Min5Bar, float64, error) {
		calls.Add(1)
		enterOnce.Do(func() { close(entered) })
		<-release
		return []datasource.Min5Bar{{Time: "202607270931", Open: 10, High: 10, Low: 10, Close: 10, Volume: 1}}, 9.9, nil
	}}

	const workers = 24
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.MinuteLine(context.Background(), "cn", "600001")
			errs <- err
		}()
	}
	close(start)
	<-entered
	// 给其余 goroutine 进入同一 flight 的机会；若未合并，它们也会进入 fetch 并累加 calls。
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发读取失败: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("同 key 并发 miss 应只拉一次上游，got %d", got)
	}
}

func TestMinuteLineLeaderCancelDoesNotCancelWaiter(t *testing.T) {
	resetMinuteLineTestState(t)

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	svc := &IntradayService{fetchMin1: func(ctx context.Context, _ string, _ string, _ int) ([]datasource.Min5Bar, float64, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return []datasource.Min5Bar{{Time: "202607270931", Open: 10, High: 10, Low: 10, Close: 10, Volume: 1}}, 9.9, nil
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := svc.MinuteLine(leaderCtx, "cn", "600001")
		leaderErr <- err
	}()
	<-entered

	waiterResult := make(chan error, 1)
	go func() {
		line, err := svc.MinuteLine(context.Background(), "cn", "600001")
		if err == nil && (line == nil || len(line.Points) != 1) {
			err = errors.New("等待者未收到完整分时结果")
		}
		waiterResult <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		minuteLineMu.Lock()
		flight := minuteLineFlights["cn:600001"]
		waiters := 0
		if flight != nil {
			waiters = flight.waiters
		}
		minuteLineMu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待者未加入同一 flight")
		}
		time.Sleep(time.Millisecond)
	}

	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader 应按自身 context 取消，got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("leader 取消后仍应复用原 flight，got %d 次上游调用", got)
	}
	close(release)
	if err := <-waiterResult; err != nil {
		t.Fatalf("仍存活的等待者不应受 leader 取消影响: %v", err)
	}
}

func TestMinuteLineCanceledFlightCannotOverwriteReplacementCache(t *testing.T) {
	resetMinuteLineTestState(t)

	const key = "cn:600001"
	var calls atomic.Int32
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	svc := &IntradayService{fetchMin1: func(context.Context, string, string, int) ([]datasource.Min5Bar, float64, error) {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst // 模拟上游在请求取消后仍迟到成功。
			return []datasource.Min5Bar{{Time: "202607270931", Open: 10, High: 10, Low: 10, Close: 10, Volume: 1}}, 9.9, nil
		case 2:
			return []datasource.Min5Bar{{Time: "202607270931", Open: 20, High: 20, Low: 20, Close: 20, Volume: 1}}, 19.9, nil
		default:
			return nil, 0, fmt.Errorf("意外的第 %d 次上游调用", calls.Load())
		}
	}}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := svc.MinuteLine(firstCtx, "cn", "600001")
		firstResult <- err
	}()
	<-firstEntered

	minuteLineMu.Lock()
	oldFlight := minuteLineFlights[key]
	minuteLineMu.Unlock()
	if oldFlight == nil {
		t.Fatal("首个 flight 未注册")
	}

	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("首个等待者应按 context 取消，got %v", err)
	}

	replacement, err := svc.MinuteLine(context.Background(), "cn", "600001")
	if err != nil {
		t.Fatalf("replacement flight 获取失败: %v", err)
	}
	if replacement.Last != 20 {
		t.Fatalf("replacement flight 应返回新数据，got %+v", replacement)
	}

	close(releaseFirst)
	<-oldFlight.done

	cached, err := svc.MinuteLine(context.Background(), "cn", "600001")
	if err != nil {
		t.Fatalf("读取 replacement 缓存失败: %v", err)
	}
	if cached.Last != 20 {
		t.Fatalf("迟到的旧 flight 不得覆盖 replacement 缓存，got %+v", cached)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("最终读取应命中 replacement 缓存，got %d 次上游调用", got)
	}
}

func TestPruneMinuteLineCacheExpiredAndBounded(t *testing.T) {
	resetMinuteLineTestState(t)
	now := time.Now()
	minuteLineMu.Lock()
	minuteLineCache = map[string]minuteLineCacheEntry{
		"expired": {at: now.Add(-minuteLineCacheTTL - time.Second), line: &MinuteLine{}},
	}
	for i := 0; i < minuteLineCacheMaxEntries+8; i++ {
		key := fmt.Sprintf("cn:%06d", i)
		minuteLineCache[key] = minuteLineCacheEntry{
			at:   now.Add(-time.Duration(minuteLineCacheMaxEntries+8-i) * time.Millisecond),
			line: &MinuteLine{},
		}
	}
	pruneMinuteLineCacheLocked(now)
	_, expiredKept := minuteLineCache["expired"]
	_, newestKept := minuteLineCache[fmt.Sprintf("cn:%06d", minuteLineCacheMaxEntries+7)]
	size := len(minuteLineCache)
	minuteLineMu.Unlock()

	if expiredKept {
		t.Fatal("过期缓存项应被清除")
	}
	if size >= minuteLineCacheMaxEntries {
		t.Fatalf("清理后应为下一次写入预留容量，size=%d max=%d", size, minuteLineCacheMaxEntries)
	}
	if !newestKept {
		t.Fatal("容量淘汰应保留最新缓存项")
	}
}
