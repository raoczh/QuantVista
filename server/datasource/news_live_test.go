package datasource

import (
	"context"
	"os"
	"testing"
	"time"
)

// 上游真实接口冒烟测试：默认跳过，LIVE_NEWS=1 时启用。
// go test ./datasource/ -run Live -v  （需设置环境变量）

func TestLiveClsTelegraph(t *testing.T) {
	if os.Getenv("LIVE_NEWS") == "" {
		t.Skip("设 LIVE_NEWS=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := GetClsTelegraph(ctx, 0, 20)
	if err != nil {
		t.Fatalf("财联社电报: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("财联社电报返回 0 条")
	}
	t.Logf("财联社 %d 条，首条: %s | %s | 重要=%v 关联=%v",
		len(items), items[0].PublishTime.Format("15:04:05"), items[0].Title, items[0].Important, items[0].Symbols)
}

func TestLiveEMFastNews(t *testing.T) {
	if os.Getenv("LIVE_NEWS") == "" {
		t.Skip("设 LIVE_NEWS=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, sortEnd, err := GetEMFastNews(ctx, "", 20)
	if err != nil {
		t.Fatalf("东财快讯: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("东财快讯返回 0 条")
	}
	t.Logf("东财快讯 %d 条 sortEnd=%s，首条: %s | %s", len(items), sortEnd,
		items[0].PublishTime.Format("15:04:05"), items[0].Title)
}

func TestLiveEMStockNews(t *testing.T) {
	if os.Getenv("LIVE_NEWS") == "" {
		t.Skip("设 LIVE_NEWS=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := GetEMStockNews(ctx, "600519", 5)
	if err != nil {
		t.Fatalf("东财个股新闻（TLS 指纹风险源，被拒则 N1 降级）: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("东财个股新闻返回 0 条")
	}
	t.Logf("个股新闻 %d 条，首条: %s | %s", len(items), items[0].PublishTime.Format("2006-01-02"), items[0].Title)
}
