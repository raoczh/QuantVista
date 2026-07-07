package datasource

import (
	"context"
	"os"
	"testing"
	"time"
)

// F1 上游真实接口冒烟测试：默认跳过，LIVE_FIN=1 时启用。
// go test ./datasource/ -run LiveFin -v  （需设置环境变量）

// TestLiveFinDataCenter datacenter 网关真实拉取：业绩预告当前报告期首页 + 翻到第二页。
func TestLiveFinDataCenter(t *testing.T) {
	if os.Getenv("LIVE_FIN") == "" {
		t.Skip("设 LIVE_FIN=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	it := NewEastMoneyAdapter().DataCenterQuery(DataCenterQuery{
		ReportName: "RPT_PUBLIC_OP_NEWPREDICT",
		Filter:     "(REPORT_DATE='2026-06-30')",
		SortColumns: "NOTICE_DATE", SortTypes: "-1",
		PageSize: 50,
	})
	p1, err := it.Next(ctx)
	if err != nil {
		t.Fatalf("业绩预告第一页: %v", err)
	}
	row, err := ParseDcRow(p1[0])
	if err != nil {
		t.Fatal(err)
	}
	if row.String("SECURITY_CODE") == "" || row.Date("NOTICE_DATE") == "" || row.String("PREDICT_TYPE") == "" {
		t.Fatalf("字段口径漂移: %v", row)
	}
	t.Logf("第一页 %d 行，首行: %s %s %s %s", len(p1),
		row.String("SECURITY_CODE"), row.String("SECURITY_NAME_ABBR"), row.String("PREDICT_TYPE"), row.Date("NOTICE_DATE"))
	p2, err := it.Next(ctx)
	if err != nil {
		t.Fatalf("翻页失败: %v", err)
	}
	t.Logf("第二页 %d 行（限流后翻页正常）", len(p2))
}

// TestLiveFinAnnouncements 公告接口真实拉取。
func TestLiveFinAnnouncements(t *testing.T) {
	if os.Getenv("LIVE_FIN") == "" {
		t.Skip("设 LIVE_FIN=1 启用真实接口冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := GetEMAnnouncements(ctx, "000001", 5)
	if err != nil {
		t.Fatalf("公告: %v", err)
	}
	if items[0].ArtCode == "" || items[0].Title == "" || items[0].URL == "" {
		t.Fatalf("公告字段口径漂移: %+v", items[0])
	}
	t.Logf("公告 %d 条，首条: %s | %s | %s", len(items), items[0].NoticeDate.Format("2006-01-02"), items[0].NoticeType, items[0].Title)
}
