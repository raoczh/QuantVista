package datasource

import (
	"context"
	"os"
	"testing"
	"time"
)

// T1 上游真实接口冒烟：默认跳过，LIVE_MARKET=1 时启用。
// go test ./datasource/ -run LiveMarket -v  （需设置环境变量）

// TestLiveMarketDailyBarsTurnover 东财日线 fields2 扩到 f61 后的实测：
// 换手率列存在且数值合理（0~100%），lmt=210 能拉满（筹码分布窗口口径）。
func TestLiveMarketDailyBarsTurnover(t *testing.T) {
	if os.Getenv("LIVE_MARKET") == "" {
		t.Skip("设 LIVE_MARKET=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bars, err := NewEastMoneyAdapter().GetDailyBars(ctx, "cn", "000001", 210)
	if err != nil {
		t.Fatalf("日线: %v", err)
	}
	if len(bars) < 200 {
		t.Fatalf("lmt=210 应拉到约 210 根，得到 %d", len(bars))
	}
	withTurnover := 0
	for _, b := range bars {
		if b.TurnoverRate < 0 || b.TurnoverRate > 100 {
			t.Fatalf("换手率越界 %s: %v", b.TradeDate, b.TurnoverRate)
		}
		if b.TurnoverRate > 0 {
			withTurnover++
		}
	}
	if withTurnover < len(bars)*9/10 {
		t.Fatalf("换手率覆盖过低（f61 口径漂移？）: %d/%d", withTurnover, len(bars))
	}
	last := bars[len(bars)-1]
	t.Logf("%d 根，末根 %s close=%v turnover=%v%%", len(bars), last.TradeDate, last.Close, last.TurnoverRate)
}

// TestLiveMarketSpotSnapshot 东财 clist 全市场快照实测（M1 增量源）：
// 沪深 A 股约 5500 只翻页拉全，行数、字段口径与停牌行容错。
func TestLiveMarketSpotSnapshot(t *testing.T) {
	if os.Getenv("LIVE_MARKET") == "" {
		t.Skip("设 LIVE_MARKET=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	rows, err := NewEastMoneyAdapter().GetCNSpotSnapshot(ctx)
	if err != nil {
		t.Fatalf("快照: %v", err)
	}
	if len(rows) < 5000 {
		t.Fatalf("全市场应约 5500 只，得到 %d", len(rows))
	}
	trading, maxTS := 0, int64(0)
	for _, r := range rows {
		if len(r.Symbol) != 6 {
			t.Fatalf("代码格式异常: %+v", r)
		}
		if r.Price > 0 && r.Volume > 0 {
			trading++
		}
		if r.DataTime > maxTS {
			maxTS = r.DataTime
		}
	}
	if trading < len(rows)*8/10 {
		t.Fatalf("有效成交行过少 %d/%d（字段口径漂移？）", trading, len(rows))
	}
	t.Logf("%d 只（有效成交 %d），max(f124)=%s，耗时 %v",
		len(rows), trading, time.Unix(maxTS, 0).Format("2006-01-02 15:04:05"), time.Since(start))
}
