package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/datasource"
)

// A1 分时图（当日走势）：腾讯 mkline m1 逐分钟线 → 前端分时图。
//
// 与 M3b 盘中因子（intraday.go）的关系：**同一上游、不同消费**——因子走 m5 盘后
// 全市场同步落库，分时图走 m1 按需拉取不落库（一天 240 根 × 5500 只没有存的价值，
// 且分时是「看当下」的东西，历史分时归档不是散户需求）。
//
// 口径与边界（前端如实展示，不粉饰）：
//   - **均价线是估算**：上游 m1 无成交额列，均价按 Σ(close×量)/Σ量 累计推导。
//     与行情软件按真实成交额计算的 VWAP 会有小幅差异，不冒充精确值；
//   - **归属交易日由末根决定**：非交易时段上游回最近交易日的根（实测 2026-07-27
//     09:20 请求回 07-24），不假设恒为「今天」，TradeDate 如实带出；
//   - **前收盘取上游 prec**：拿不到时（prec 缺失/为 0）回退首根开盘价并标记
//     BaseFromOpen=true，前端据此说明基准线口径，不静默拿开盘价冒充昨收；
//   - 上游给的末根在盘中是半截 bar（进行中），这是分时图的正常语义不做剔除。

const (
	// minuteLineCacheTTL 分时图进程内缓存时长。盘中前端可能高频刷新/多标签页打开，
	// 无缓存会逐次打上游（腾讯域虽不经断路器，被刷同样有封 IP 风险）。
	// 60s 对分时图足够（一根 bar 就是 60s）。
	minuteLineCacheTTL = 60 * time.Second
	// flight 不绑定任一 HTTP 请求；所有等待者都离开时才取消。腾讯公共 HTTP 客户端
	// 自带 8s 超时，这里再设服务层上限，防测试替身或未来 adapter 漏配超时。
	minuteLineFetchTimeout = 10 * time.Second
	// minuteLineCacheMaxEntries 限制缓存常驻内存。每条含约 240 个分时点，
	// 即使短时间访问大量不同标的也不得无限增长。
	minuteLineCacheMaxEntries = 512

	// minuteLineFetchCount 单次拉取根数：一天 240 根 + 余量，保证整日不被截断；
	// 余量同时覆盖「今日尚未开盘、上游回上一交易日整日」的情形。
	minuteLineFetchCount = 260
)

// MinutePoint 分时图单点。
type MinutePoint struct {
	Time   string  `json:"time"`   // HH:mm（bar 结束时刻）
	Price  float64 `json:"price"`  // 该分钟收盘价
	Avg    float64 `json:"avg"`    // 累计估算均价（见文件头口径声明）
	Volume int64   `json:"volume"` // 该分钟成交量（手）
}

// MinuteLine 分时图响应。
type MinuteLine struct {
	Symbol    string  `json:"symbol"`
	Market    string  `json:"market"`
	TradeDate string  `json:"trade_date"` // 数据归属交易日（由末根决定，非必然是今天）
	PrevClose float64 `json:"prev_close"` // 基准线（昨收）
	// BaseFromOpen=true 表示上游未给昨收，PrevClose 用首根开盘价兜底（前端须说明）。
	BaseFromOpen bool          `json:"base_from_open"`
	Points       []MinutePoint `json:"points"`
	TotalVolume  int64         `json:"total_volume"` // 当日累计成交量（手）
	High         float64       `json:"high"`         // 当日最高（分时口径）
	Low          float64       `json:"low"`          // 当日最低
	Last         float64       `json:"last"`         // 末根价
	// AvgNote 均价口径声明，前端原样展示（禁止前端自行编造口径说明）。
	AvgNote string `json:"avg_note"`
}

// minuteLineCacheEntry 缓存单元。
type minuteLineCacheEntry struct {
	at   time.Time
	line *MinuteLine
}

// minuteLineFlight 合并同一标的的并发 cache miss，避免首屏多请求同时打上游。
type minuteLineFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	line    *MinuteLine
	err     error
}

var (
	minuteLineMu      sync.Mutex
	minuteLineCache   = map[string]minuteLineCacheEntry{}
	minuteLineFlights = map[string]*minuteLineFlight{}
)

// pruneMinuteLineCacheLocked 清除过期项，并为下一次写入预留一个位置。
// 调用方必须持有 minuteLineMu。
func pruneMinuteLineCacheLocked(now time.Time) {
	for key, entry := range minuteLineCache {
		if now.Sub(entry.at) >= minuteLineCacheTTL {
			delete(minuteLineCache, key)
		}
	}
	if len(minuteLineCache) < minuteLineCacheMaxEntries {
		return
	}
	type cacheAge struct {
		key string
		at  time.Time
	}
	ages := make([]cacheAge, 0, len(minuteLineCache))
	for key, entry := range minuteLineCache {
		ages = append(ages, cacheAge{key: key, at: entry.at})
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].at.Before(ages[j].at) })
	remove := len(minuteLineCache) - minuteLineCacheMaxEntries + 1
	for i := 0; i < remove; i++ {
		delete(minuteLineCache, ages[i].key)
	}
}

// buildMinuteLine 由升序 m1 根与前收盘构建分时图（纯函数，单测锚点）。
// 只取**末根所属交易日**的根——上游一次可能回跨日的根（如 07-24 尾部 + 07-25 全天）。
func buildMinuteLine(market, symbol string, bars []datasource.Min5Bar, prec float64) (*MinuteLine, error) {
	if len(bars) == 0 {
		return nil, datasource.ErrNoData
	}
	sorted := make([]datasource.Min5Bar, len(bars))
	copy(sorted, bars)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })

	day := min5Date(sorted[len(sorted)-1].Time)
	if day == "" {
		return nil, datasource.ErrNoData
	}
	out := &MinuteLine{
		Symbol: symbol, Market: market, TradeDate: day,
		PrevClose: prec,
		AvgNote:   "均价为估算值（上游分钟线无成交额，按 Σ(close×量)/Σ量 推导），与行情软件 VWAP 或有小幅差异",
	}
	var cumAmount float64 // Σ(close × 量)
	var cumVol int64
	for _, b := range sorted {
		if min5Date(b.Time) != day {
			continue // 跨日根：只保留末根所属交易日
		}
		if b.Volume > 0 {
			cumAmount += b.Close * float64(b.Volume)
			cumVol += b.Volume
		}
		avg := b.Close // 全天零成交（停牌/无量）时均价退化为现价，不产生 0 值折线
		if cumVol > 0 {
			avg = cumAmount / float64(cumVol)
		}
		out.Points = append(out.Points, MinutePoint{
			Time:   formatMinuteClock(b.Time),
			Price:  round3(b.Close),
			Avg:    round3(avg),
			Volume: b.Volume,
		})
		if out.High == 0 || b.High > out.High {
			out.High = b.High
		}
		if out.Low == 0 || b.Low < out.Low {
			out.Low = b.Low
		}
	}
	if len(out.Points) == 0 {
		return nil, datasource.ErrNoData
	}
	out.TotalVolume = cumVol
	out.Last = out.Points[len(out.Points)-1].Price
	// 前收盘兜底：上游 prec 缺失时用当日首根开盘价，并显式标记口径（不静默冒充昨收）。
	if out.PrevClose <= 0 {
		for _, b := range sorted {
			if min5Date(b.Time) == day {
				out.PrevClose = round3(b.Open)
				out.BaseFromOpen = true
				break
			}
		}
	} else {
		out.PrevClose = round3(out.PrevClose)
	}
	return out, nil
}

// formatMinuteClock YYYYMMDDHHmm → HH:mm。
func formatMinuteClock(t string) string {
	c := min5Clock(t)
	if len(c) != 4 {
		return c
	}
	return c[:2] + ":" + c[2:]
}

// round3 复用 indicator.go 的实现（保留 3 位；A 股价格 2 位，均价推导出 3 位更精确）。

func waitMinuteLineFlight(ctx context.Context, key string, flight *minuteLineFlight) (*MinuteLine, error) {
	select {
	case <-flight.done:
		return flight.line, flight.err
	case <-ctx.Done():
		minuteLineMu.Lock()
		if current, ok := minuteLineFlights[key]; ok && current == flight {
			flight.waiters--
			if flight.waiters == 0 {
				// 立即移除已无人等待的 flight，让后来的请求可以新建一次取数；旧 goroutine
				// 完成时会按指针比对，不能误删新的 flight。
				delete(minuteLineFlights, key)
				flight.cancel()
			}
		}
		minuteLineMu.Unlock()
		return nil, fmt.Errorf("分时数据获取失败: %w", ctx.Err())
	}
}

func (s *IntradayService) fetchMinuteLineFlight(ctx context.Context, key, market, symbol string,
	flight *minuteLineFlight) {
	defer flight.cancel()
	bars, prec, err := s.fetchMin1(ctx, market, symbol, minuteLineFetchCount)
	if err != nil {
		if errors.Is(err, datasource.ErrNoData) || errors.Is(err, datasource.ErrSymbolInvalid) {
			err = errors.New("该标的暂无分时数据")
		} else {
			err = fmt.Errorf("分时数据获取失败: %w", err)
		}
	} else {
		flight.line, err = buildMinuteLine(market, symbol, bars, prec)
		if err != nil {
			err = errors.New("该标的暂无分时数据")
		}
	}

	minuteLineMu.Lock()
	current := minuteLineFlights[key]
	if err == nil && current == flight {
		now := time.Now()
		pruneMinuteLineCacheLocked(now)
		minuteLineCache[key] = minuteLineCacheEntry{at: now, line: flight.line}
	}
	flight.err = err
	if current == flight {
		delete(minuteLineFlights, key)
	}
	close(flight.done)
	minuteLineMu.Unlock()
}

// MinuteLine 取个股分时图（带 60s 进程内缓存）。仅 A 股。
func (s *IntradayService) MinuteLine(ctx context.Context, market, symbol string) (*MinuteLine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	market = strings.ToLower(strings.TrimSpace(market))
	symbol = strings.TrimSpace(symbol)
	if market != "cn" {
		return nil, errors.New("分时图仅支持 A 股（market=cn）")
	}
	if symbol == "" {
		return nil, errors.New("缺少股票代码")
	}
	key := market + ":" + symbol

	minuteLineMu.Lock()
	now := time.Now()
	if e, ok := minuteLineCache[key]; ok && now.Sub(e.at) < minuteLineCacheTTL {
		minuteLineMu.Unlock()
		return e.line, nil
	}
	pruneMinuteLineCacheLocked(now)
	if flight, ok := minuteLineFlights[key]; ok {
		flight.waiters++
		minuteLineMu.Unlock()
		return waitMinuteLineFlight(ctx, key, flight)
	}
	fetchCtx, cancel := context.WithTimeout(context.Background(), minuteLineFetchTimeout)
	flight := &minuteLineFlight{done: make(chan struct{}), cancel: cancel, waiters: 1}
	minuteLineFlights[key] = flight
	minuteLineMu.Unlock()

	go s.fetchMinuteLineFlight(fetchCtx, key, market, symbol, flight)
	return waitMinuteLineFlight(ctx, key, flight)
}
