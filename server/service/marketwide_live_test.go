package service

import (
	"context"
	"os"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// M1 全市场日线真实验收（内存库 + 真实东财接口）：默认跳过，LIVE_WIDE=1 启用。
// go test ./service/ -run TestLiveWide -v -timeout 15m
//
// 覆盖验收项：全市场增量同步实测跑通；历史初始化断点续传实测（跑一段暂停 → 续跑推进、
// 已完成标的不重拉）。除权重锚的真实命中依赖除权日，无法按需实测，由单测 + 部署观察兜底。
func TestLiveWideSyncAndInitResume(t *testing.T) {
	if os.Getenv("LIVE_WIDE") == "" {
		t.Skip("设 LIVE_WIDE=1 启用真实全市场增量+初始化断点实测")
	}
	setupTestDB(t)
	cleanWideTables(t)
	svc := &MarketService{wide: datasource.NewEastMoneyAdapter()}

	// 1. 全市场增量：真实 clist 快照落当日 bar + 建宇宙字典。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	log, err := svc.SyncMarketWide(ctx)
	if err != nil {
		t.Fatalf("增量同步: %v", err)
	}
	t.Logf("增量: %s（%dms）", log.Message, log.DurationMs)
	if log.Succeeded < 4500 {
		t.Fatalf("落 bar 数过少: %d", log.Succeeded)
	}
	var pending int64
	common.DB.Model(&model.MarketSyncState{}).Where("init_status = ?", "pending").Count(&pending)
	if pending < 5000 {
		t.Fatalf("宇宙字典 pending = %d, want ≥5000", pending)
	}

	// 2. 历史初始化第一段：跑 25 秒即取消（模拟暂停）。
	// 本机 push2his 常年被限流（EOF）：源故障中止路径触发时诚实 Skip，部署环境重跑。
	ctx1, cancel1 := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel1()
	log1, _ := svc.initMarketWideHistory(ctx1)
	var done1 int64
	common.DB.Model(&model.MarketSyncState{}).Where("init_status = ?", "done").Count(&done1)
	t.Logf("第一段 25s：done=%d", done1)
	if done1 == 0 {
		if log1 != nil && log1.Status == "failed" {
			t.Skipf("东财日线源不可用（本机 push2his 被限流），初始化断点实测留部署环境: %s", log1.Message)
		}
		t.Fatal("第一段应至少完成若干只")
	}
	var st model.MarketSyncState
	common.DB.Where("init_status = ?", "done").Order("id").First(&st)
	if st.BarsCount < 200 || st.LastBarDate == "" {
		t.Fatalf("已完成标的落库不符: %+v", st)
	}
	if n := barCount(t, st.Symbol); n < 200 {
		t.Fatalf("%s bar 数 = %d, want ≥200", st.Symbol, n)
	}

	// 3. 续跑第二段：从断点推进（done 增加），且已 done 的不被重复处理。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel2()
	log2, _ := svc.initMarketWideHistory(ctx2)
	var done2 int64
	common.DB.Model(&model.MarketSyncState{}).Where("init_status = ?", "done").Count(&done2)
	t.Logf("第二段 25s：done=%d（+%d），本段处理 %d", done2, done2-done1, log2.Total)
	if done2 <= done1 {
		t.Fatal("续跑应从断点继续推进")
	}
	if int64(log2.Total) > done2-done1+int64(log2.Failed) {
		t.Fatalf("续跑重复处理了已 done 标的: 本段 %d, 新增 %d", log2.Total, done2-done1)
	}
}
