package service

import (
	"context"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func resetStockSearchTables(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	for _, table := range []any{
		&model.StockUniverseDaily{},
		&model.MarketSyncState{},
		&model.WatchlistItem{},
		&model.Position{},
	} {
		if err := common.DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(table).Error; err != nil {
			t.Fatalf("清空搜索测试表失败: %v", err)
		}
	}
}

func createUniverseRows(t *testing.T, rows ...model.StockUniverseDaily) {
	t.Helper()
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatalf("写入股票池快照失败: %v", err)
	}
}

func TestStockSearchUsesLatestSingleUniverseSnapshot(t *testing.T) {
	resetStockSearchTables(t)
	createUniverseRows(t,
		model.StockUniverseDaily{TradeDate: "2026-07-31", Symbol: "000001", Market: "cn", Name: "旧日股份", Industry: "旧行业"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "600519", Market: "cn", Name: "贵州股份", Industry: "白酒"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "600000", Market: "cn", Name: "浦发股份", Industry: "银行"},
	)

	result, err := NewStockSearchService().Search(context.Background(), 1, "股份", 20)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if result.Source != stockSearchSourceUniverse || result.AsOf != "2026-08-01" {
		t.Fatalf("应使用最新单日快照: %+v", result)
	}
	if len(result.Items) != 2 {
		t.Fatalf("不得混入旧日期股票，得到 %+v", result.Items)
	}
	if result.Items[0].Symbol != "600000" || result.Items[1].Symbol != "600519" {
		t.Fatalf("同等级结果应按代码稳定排序: %+v", result.Items)
	}
	for _, item := range result.Items {
		if item.AsOf != "2026-08-01" {
			t.Fatalf("结果日期不一致: %+v", item)
		}
	}
	if result.Items[1].Industry != "白酒" {
		t.Fatalf("应返回快照行业: %+v", result.Items[1])
	}
}

func TestStockSearchFallsBackToMarketSyncState(t *testing.T) {
	resetStockSearchTables(t)
	if err := common.DB.Create(&model.MarketSyncState{
		Symbol: "600000", Market: "cn", Name: "浦发银行", LastBarDate: "2026-08-03", InitStatus: "done",
	}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := NewStockSearchService().Search(context.Background(), 1, "pfyh", 20)
	if err != nil {
		t.Fatalf("降级搜索失败: %v", err)
	}
	if result.Source != stockSearchSourceFallback || result.AsOf != "2026-08-03" {
		t.Fatalf("降级源或日期错误: %+v", result)
	}
	if len(result.Items) != 1 || result.Items[0].Symbol != "600000" || result.Items[0].AsOf != "2026-08-03" {
		t.Fatalf("首字母降级搜索错误: %+v", result.Items)
	}
}

func TestStockSearchFallbackAsOfUsesUpdatedAt(t *testing.T) {
	resetStockSearchTables(t)
	state := model.MarketSyncState{Symbol: "600001", Market: "cn", Name: "初始化股票", InitStatus: "pending"}
	if err := common.DB.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	wantDate := state.UpdatedAt.Format("2006-01-02")

	result, err := NewStockSearchService().Search(context.Background(), 1, "600001", 20)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != stockSearchSourceFallback || result.AsOf != wantDate ||
		len(result.Items) != 1 || result.Items[0].AsOf != wantDate {
		t.Fatalf("无日线降级日期应使用字典更新时间: %+v", result)
	}

	noMatch, err := NewStockSearchService().Search(context.Background(), 1, "不存在", 20)
	if err != nil {
		t.Fatal(err)
	}
	if noMatch.Source != stockSearchSourceFallback || noMatch.AsOf != wantDate || len(noMatch.Items) != 0 {
		t.Fatalf("无命中也应保留降级源元信息: %+v", noMatch)
	}
}

func TestStockSearchRankingAndStableOrder(t *testing.T) {
	resetStockSearchTables(t)
	const day = "2026-08-01"
	createUniverseRows(t,
		model.StockUniverseDaily{TradeDate: day, Symbol: "PINGAN", Market: "cn", Name: "代码精确"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "PINGAN2", Market: "cn", Name: "代码前缀"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100003", Market: "cn", Name: "pingan"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100004", Market: "cn", Name: "pingan科技"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100005", Market: "cn", Name: "平安银行"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100006", Market: "cn", Name: "研究pingan样本"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100010", Market: "cn", Name: "墨龙股份"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100011", Market: "cn", Name: "明欧科技"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "100012", Market: "cn", Name: "重庆银行"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "200002", Market: "cn", Name: "Beta research"},
		model.StockUniverseDaily{TradeDate: day, Symbol: "200001", Market: "cn", Name: "Alpha research"},
	)

	result, err := NewStockSearchService().Search(context.Background(), 1, "pingan", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PINGAN", "PINGAN2", "100003", "100004", "100005", "100006"}
	assertSearchSymbols(t, result.Items, want)

	result, err = NewStockSearchService().Search(context.Background(), 1, "mo", 20)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchSymbols(t, result.Items, []string{"100010", "100011"})

	result, err = NewStockSearchService().Search(context.Background(), 1, "chongqingyinhang", 20)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchSymbols(t, result.Items, []string{"100012"})

	result, err = NewStockSearchService().Search(context.Background(), 1, "research", 20)
	if err != nil {
		t.Fatal(err)
	}
	assertSearchSymbols(t, result.Items, []string{"200001", "200002"})
}

func TestStockSearchUserRelationsAreIsolated(t *testing.T) {
	resetStockSearchTables(t)
	createUniverseRows(t,
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "600001", Market: "cn", Name: "关系股份甲"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "600002", Market: "cn", Name: "关系股份乙"},
	)
	items := []model.WatchlistItem{
		{UserID: 1, WatchlistID: 1, Symbol: "600001", Market: "cn", Name: "关系股份甲"},
		{UserID: 2, WatchlistID: 2, Symbol: "600002", Market: "cn", Name: "关系股份乙"},
	}
	if err := common.DB.Create(&items).Error; err != nil {
		t.Fatal(err)
	}
	positions := []model.Position{
		{UserID: 1, Symbol: "600002", Market: "cn", Name: "关系股份乙", Status: model.PositionStatusHolding, Quantity: 100},
		{UserID: 1, Symbol: "600001", Market: "cn", Name: "关系股份甲", Status: model.PositionStatusClosed, Quantity: 0},
		{UserID: 2, Symbol: "600001", Market: "cn", Name: "关系股份甲", Status: model.PositionStatusHolding, Quantity: 100},
	}
	if err := common.DB.Create(&positions).Error; err != nil {
		t.Fatal(err)
	}

	user1, err := NewStockSearchService().Search(context.Background(), 1, "关系股份", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(user1.Items) != 2 || !user1.Items[0].InWatchlist || user1.Items[0].HasPosition ||
		user1.Items[1].InWatchlist || !user1.Items[1].HasPosition {
		t.Fatalf("用户 1 关系错误或泄漏: %+v", user1.Items)
	}

	user2, err := NewStockSearchService().Search(context.Background(), 2, "关系股份", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(user2.Items) != 2 || user2.Items[0].InWatchlist || !user2.Items[0].HasPosition ||
		!user2.Items[1].InWatchlist || user2.Items[1].HasPosition {
		t.Fatalf("用户 2 关系错误或泄漏: %+v", user2.Items)
	}
}

func TestStockSearchLimitsAndLiteralLikeWildcards(t *testing.T) {
	resetStockSearchTables(t)
	createUniverseRows(t,
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "300001", Market: "cn", Name: "百分%科技"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "300002", Market: "cn", Name: "下划_线"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "300003", Market: "cn", Name: "惊叹!科技"},
		model.StockUniverseDaily{TradeDate: "2026-08-01", Symbol: "300004", Market: "cn", Name: "普通科技"},
	)
	svc := NewStockSearchService()
	for query, wantSymbol := range map[string]string{"%": "300001", "_": "300002", "!": "300003"} {
		result, err := svc.Search(context.Background(), 1, query, 20)
		if err != nil {
			t.Fatalf("LIKE 字面量 %q 搜索失败: %v", query, err)
		}
		assertSearchSymbols(t, result.Items, []string{wantSymbol})
	}
	injected, err := svc.Search(context.Background(), 1, "%' OR 1=1 --", 20)
	if err != nil {
		t.Fatalf("注入载荷应作为普通文本处理: %v", err)
	}
	if len(injected.Items) != 0 {
		t.Fatalf("注入载荷不得扩大结果集: %+v", injected.Items)
	}
	if _, err := svc.Search(context.Background(), 1, "   ", 20); err == nil {
		t.Fatal("空 q 应被拒绝")
	}
	if _, err := svc.Search(context.Background(), 1, "科技", 0); err == nil {
		t.Fatal("limit=0 应被拒绝")
	}
	if _, err := svc.Search(context.Background(), 1, "科技", StockSearchMaxLimit+1); err == nil {
		t.Fatal("超过最大 limit 应被拒绝")
	}
	if _, err := svc.Search(context.Background(), 1, strings.Repeat("股", stockSearchMaxQueryLen+1), 20); err == nil {
		t.Fatal("过长 q 应被拒绝")
	}
	limited, err := svc.Search(context.Background(), 1, "科技", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Items) != 2 {
		t.Fatalf("limit 未生效: %+v", limited.Items)
	}
}

func TestStockSearchNoDataReturnsEmptyItems(t *testing.T) {
	resetStockSearchTables(t)
	result, err := NewStockSearchService().Search(context.Background(), 1, "600519", 20)
	if err != nil {
		t.Fatalf("无数据不应报错: %v", err)
	}
	if result.Items == nil || len(result.Items) != 0 || result.Source != "" || result.AsOf != "" {
		t.Fatalf("无数据响应应稳定为空数组: %+v", result)
	}
}

func assertSearchSymbols(t *testing.T, items []StockSearchItem, want []string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("结果数量=%d，期望=%d，结果=%+v", len(items), len(want), items)
	}
	for i := range want {
		if items[i].Symbol != want[i] {
			t.Fatalf("第 %d 项代码=%s，期望=%s，结果=%+v", i, items[i].Symbol, want[i], items)
		}
	}
}
