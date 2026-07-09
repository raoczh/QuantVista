package service

import (
	"context"
	"os"
	"testing"
	"time"
)

// 真实上游冒烟（默认跳过）：LIVE_MOOD=1 go test ./service/ -run TestLiveMood -v
// 验证龙虎榜/机构统计/涨停池/人气榜/资金流历史五路真实接口与端到端落库。
// 注意：涨停池只对最近交易日有效；龙虎榜 18:00 前当日可能未发布（自动回退上一交易日）。
func TestLiveMood(t *testing.T) {
	if os.Getenv("LIVE_MOOD") == "" {
		t.Skip("需要 LIVE_MOOD=1 才跑真实接口冒烟")
	}
	setupTestDB(t)
	cleanMoodTables(t)
	svc := NewMoodService()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	poolDate := moodTargetDate(time.Now(), moodPoolCutoffMin)
	if err := svc.SyncZTPools(ctx, poolDate); err != nil {
		// 涨停池上游不可回溯：目标日非今日时（盘中跑冒烟）数据已翻页，预期缺失。
		if poolDate != time.Now().Format("2006-01-02") {
			t.Logf("涨停池 %s 不可回溯（预期，盘后当日采集才有效）: %v", poolDate, err)
		} else {
			t.Errorf("涨停池采集失败 %s: %v", poolDate, err)
		}
	} else {
		t.Logf("涨停池 %s: %+v", poolDate, moodBrief())
	}
	if err := svc.SyncPopularity(ctx, time.Now().Format("2006-01-02")); err != nil {
		t.Errorf("人气榜采集失败: %v", err)
	}
	lhbDate := moodTargetDate(time.Now(), moodLhbCutoffMin)
	if n, err := svc.SyncLhb(ctx, lhbDate); err != nil {
		t.Errorf("龙虎榜采集失败 %s: %v", lhbDate, err)
	} else {
		t.Logf("龙虎榜 %s: %d 行", lhbDate, n)
	}
	if flows, fresh := ensureStockFundFlow(ctx, svc.em, "cn", "600519", nil); len(flows) == 0 {
		t.Error("茅台资金流历史应非空")
	} else {
		t.Logf("600519 资金流 %d 根 fresh=%v 连续=%d", len(flows), fresh, mainNetStreakDays(flows))
	}
	if rows, err := svc.em.GetFundFlowRank(ctx, "f62", 10); err != nil {
		t.Errorf("资金流排行失败: %v", err)
	} else {
		t.Logf("资金流排行 top1: %s %s 主力净额 %.2f 亿", rows[0].Symbol, rows[0].Name, rows[0].MainNet/1e8)
	}
}
