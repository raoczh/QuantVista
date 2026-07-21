package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestScoreNewsEvent(t *testing.T) {
	mk := func(title string) model.News {
		return model.News{Title: title, SourcePriority: 1}
	}
	// 中央级 + 全市场 + 资金直接：满配。
	src, imp, fund := scoreNewsEvent(mk("国务院部署降准，释放流动性支持A股市场"))
	if src != 5 || imp != 5 || fund != 5 {
		t.Errorf("满配事件 = (%d,%d,%d), want (5,5,5)", src, imp, fund)
	}
	// 情绪增强字段优先于关键词推断。
	n := mk("某消息")
	n.PolicyLevel = 4
	n.ImpactScope = "sector"
	src, imp, _ = scoreNewsEvent(n)
	if src != 4 || imp != 3 {
		t.Errorf("增强字段优先: (%d,%d), want (4,3)", src, imp)
	}
	// 个股级普通消息：低分。
	n2 := mk("某公司签订日常经营合同")
	n2.SourcePriority = 2
	n2.RelatedSymbols = `["600001"]`
	src, imp, fund = scoreNewsEvent(n2)
	if src+imp+fund >= eventKeepScore {
		t.Errorf("个股普通消息不应过保留线: %d+%d+%d", src, imp, fund)
	}
	// 交易所级。
	if src, _, _ := scoreNewsEvent(mk("上交所发布程序化交易新规")); src < 3 {
		t.Errorf("交易所级 src=%d, want ≥3", src)
	}
}

func TestSelectReportEvents(t *testing.T) {
	now := time.Now()
	mk := func(title string, prio int, sectors string) model.News {
		return model.News{Title: title, SourcePriority: prio, PublishTime: now, RelatedSectors: sectors}
	}
	rows := []model.News{
		mk("收评：三大指数集体收涨，两市成交额1.2万亿", 1, ""), // 黑名单降噪
		mk("央行宣布降准0.5个百分点，释放长期流动性约1万亿元", 1, `["银行"]`),
		mk("央行降准0.5个百分点释放约1万亿元流动性", 2, `["银行"]`), // 同主线，应被合并
		mk("某公司中标日常项目", 3, ""),                   // 低分被过滤
		mk("证监会就减持新规公开征求意见", 1, ""),
	}
	events := selectReportEvents(rows)
	if len(events) != 2 {
		t.Fatalf("应保留 2 条(降准合并+减持新规), got %d: %+v", len(events), events)
	}
	// 降准事件：重磅 + 合并计数。
	var jiangzhun *reportEvent
	for i := range events {
		if events[i].Merged > 0 {
			jiangzhun = &events[i]
		}
	}
	if jiangzhun == nil {
		t.Fatalf("同主线合并未生效: %+v", events)
	}
	if !jiangzhun.Major {
		t.Errorf("降准应标重磅(≥%d): %+v", eventMajorScore, jiangzhun)
	}
	// 排序：分高在前。
	if events[0].Score < events[1].Score {
		t.Errorf("应按分降序: %+v", events)
	}
	// 打分明细齐全（透明可查）。
	for _, e := range events {
		if e.Score != e.SrcLevel+e.Impact+e.FundSens {
			t.Errorf("打分明细不自洽: %+v", e)
		}
	}
}

func TestSelectReportEventsTopN(t *testing.T) {
	now := time.Now()
	titles := []string{
		"国务院常务会议部署稳增长一揽子增量政策",
		"央行宣布降息10个基点并开展逆回购操作",
		"财政部拟发行超长期特别国债支持基建投资",
		"证监会发布上市公司市值管理指引征求意见",
		"发改委批复多项重大铁路项目投资计划",
		"中央政治局会议定调适度宽松的货币政策",
		"商务部宣布对部分商品出口管制措施调整",
		"金融监管总局放宽险资权益投资比例上限",
		"工信部印发算力基础设施高质量发展行动计划",
		"国家统计局公布CPI同比数据，宏观经济企稳",
		"住建部推进城中村改造货币化安置扩围",
		"税务总局明确减持股份个人所得税征管口径",
		"上交所下调股票交易经手费收费标准",
		"深交所优化再融资审核安排支持科技企业",
	}
	rows := make([]model.News, 0, len(titles))
	for i, title := range titles {
		rows = append(rows, model.News{
			Title: title, SourcePriority: 1,
			PublishTime: now.Add(time.Duration(i) * time.Minute),
		})
	}
	events := selectReportEvents(rows)
	if len(events) != eventTopN {
		t.Errorf("14 条独立高分事件应截断到 Top %d, got %d", eventTopN, len(events))
	}
}

func TestBuildTodayEventsExcludesFutureRecords(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.News{})
	t.Cleanup(func() { common.DB.Where("1 = 1").Delete(&model.News{}) })
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.Local)
	rows := []model.News{
		{Title: "央行宣布降准释放流动性", Source: "cls", Category: "telegraph", SourcePriority: 1, PublishTime: now.Add(-time.Hour), ContentHash: "event-past"},
		{Title: "国务院宣布降息支持市场", Source: "cls", Category: "telegraph", SourcePriority: 1, PublishTime: now.Add(time.Hour), ContentHash: "event-future"},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	events := buildTodayEventsAt("2026-07-21", now)
	if len(events) != 1 || events[0].Title != rows[0].Title {
		t.Fatalf("日报事件窗口不得包含当前时刻之后的记录: %+v", events)
	}
}
