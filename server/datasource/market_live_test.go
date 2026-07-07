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
