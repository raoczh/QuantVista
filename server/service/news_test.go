package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestNormalizeNewsTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"【重磅】央行降准 0.5 个百分点！", "重磅央行降准05个百分点"},
		{"  Hello, World 123 ", "helloworld123"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeNewsTitle(c.in); got != c.want {
			t.Errorf("normalizeNewsTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBigramDice(t *testing.T) {
	cases := []struct {
		a, b string
		min  float64
		max  float64
	}{
		{"央行宣布全面降准05个百分点", "央行宣布全面降准05个百分点", 1, 1},
		{"央行宣布全面降准05个百分点", "央行宣布全面降准05个百分点释放利好", 0.85, 1}, // 尾部增删应判重
		{"央行宣布全面降准05个百分点", "央行全面降准05个百分点", 0.7, 0.85},     // 改动较大不到阈值（现状锚定）
		{"央行宣布全面降准05个百分点", "证监会发布减持新规征求意见", 0, 0.3},       // 无关不应误伤
		{"a", "a", 1, 1},
		{"a", "b", 0, 0},
	}
	for _, c := range cases {
		got := bigramDice(c.a, c.b)
		if got < c.min || got > c.max {
			t.Errorf("bigramDice(%q,%q) = %.3f, want [%.2f,%.2f]", c.a, c.b, got, c.min, c.max)
		}
	}
}

func TestNewsContentHash(t *testing.T) {
	h1 := newsContentHash("t", "abc")
	if len(h1) != 32 {
		t.Fatalf("hash 长度 = %d, want 32", len(h1))
	}
	// 正文只取前 500 字：第 501 字起的差异不影响指纹。
	long := make([]rune, 600)
	for i := range long {
		long[i] = '字'
	}
	a := string(long)
	long[599] = '异'
	b := string(long)
	if newsContentHash("t", a) != newsContentHash("t", b) {
		t.Error("超出 500 字的差异不应影响 content_hash")
	}
	long[100] = '异'
	if newsContentHash("t", a) == newsContentHash("t", string(long)) {
		t.Error("前 500 字内的差异应改变 content_hash")
	}
}

func TestDedupeCheck(t *testing.T) {
	s := NewNewsService()
	now := time.Now()

	if s.dedupeCheck("cls", "1", "央行宣布全面降准05个百分点", now) {
		t.Fatal("首条不应判重")
	}
	if !s.dedupeCheck("cls", "1", "完全不同的标题也该被ID拦住", now) {
		t.Error("同 source:id 应判重")
	}
	if !s.dedupeCheck("eastmoney", "9", "央行宣布全面降准05个百分点", now) {
		t.Error("跨源同标题应被 title_hash 拦住")
	}
	if !s.dedupeCheck("eastmoney", "10", "央行宣布全面降准05个百分点释放利好", now) {
		t.Error("跨源相似标题（Dice≥0.85）应判重")
	}
	if s.dedupeCheck("eastmoney", "11", "证监会发布减持新规征求意见", now) {
		t.Error("无关标题不应误判")
	}
}

func TestDedupeCacheCap(t *testing.T) {
	s := NewNewsService()
	now := time.Now()
	for i := 0; i < newsSeenCap; i++ {
		s.seen[string(rune('a'+i%26))+string(rune(i))] = now
	}
	if s.dedupeCheck("cls", "x", "缓存超限后仍能正常登记新条目", now) {
		t.Error("新条目不应判重")
	}
	if len(s.seen) > newsSeenCap {
		t.Errorf("缓存应砍半控制在上限内, got %d", len(s.seen))
	}
}

// TestNewsInsertFailNoRegister 写库失败不登记去重：insertNews 成功后才登记（游标同理
// 只推进到已入库/确认重复条目），失败条目不占去重名额，靠下轮重叠窗重采不丢失。
func TestNewsInsertFailNoRegister(t *testing.T) {
	setupTestDB(t)
	common.DB.Where("1 = 1").Delete(&model.News{})
	t.Cleanup(func() { common.DB.AutoMigrate(&model.News{}); common.DB.Where("1 = 1").Delete(&model.News{}) })
	s := NewNewsService()
	now := time.Now()

	// 正常路径：判重（false）→ 入库成功 → 登记 → 再判重为 true。
	if s.dedupeSeen("cls", "1", "标题甲事件") {
		t.Fatal("首条不应判重")
	}
	n := &model.News{Title: "标题甲事件", Source: "cls", SourceID: "1",
		ContentHash: newsContentHash("标题甲事件", "x"), PublishTime: now, CollectTime: now}
	if created, err := insertNews(t.Context(), n); !created || err != nil {
		t.Fatalf("首次入库应成功: %v", err)
	}
	s.dedupeRegister("cls", "1", "标题甲事件", now)
	if !s.dedupeSeen("cls", "1", "标题甲事件") {
		t.Fatal("入库成功登记后应判重")
	}

	// 写库失败：删表让 insertNews 返回 false → 不登记 → 同条目仍不判重（下轮可重采）。
	if err := common.DB.Migrator().DropTable(&model.News{}); err != nil {
		t.Fatalf("drop news: %v", err)
	}
	t.Cleanup(func() {
		if err := common.DB.AutoMigrate(&model.News{}); err != nil {
			t.Errorf("restore news: %v", err)
		}
	})
	if s.dedupeSeen("cls", "2", "标题乙事件") {
		t.Fatal("新条目不应判重")
	}
	n2 := &model.News{Title: "标题乙事件", Source: "cls", SourceID: "2",
		ContentHash: newsContentHash("标题乙事件", "y"), PublishTime: now, CollectTime: now}
	if created, err := insertNews(t.Context(), n2); created || err == nil {
		t.Fatalf("表已删，入库应返回错误: %v", err)
	}
	// 关键断言：写库失败未登记去重，同条目仍判为「未见过」，下轮重采不丢。
	if s.dedupeSeen("cls", "2", "标题乙事件") {
		t.Fatal("写库失败不应登记去重（否则该条永久丢失）")
	}
}

// TestListNewsFillsRelatedStockNames 快讯关联标的必须补全名称：原始快讯只记 6 位代码，
// 名称来自本地字典（stocks 优先，未覆盖的回落 market_sync_states）。两张表都查不到的
// 留空串，由前端按纯代码展示——对快讯而言名称是附加信息，缺失是常态，不能显示成
// 「名称待补全」占满一行。
func TestListNewsFillsRelatedStockNames(t *testing.T) {
	setupTestDB(t)
	for _, m := range []any{&model.News{}, &model.Stock{}, &model.MarketSyncState{}} {
		common.DB.Where("1 = 1").Delete(m)
	}
	t.Cleanup(func() {
		for _, m := range []any{&model.News{}, &model.Stock{}, &model.MarketSyncState{}} {
			common.DB.Where("1 = 1").Delete(m)
		}
	})

	// 600519 只在 stocks；000002 只在宇宙字典（验证回落）；999999 两处都没有。
	common.DB.Create(&model.Stock{Symbol: "600519", Market: "cn", Name: "贵州茅台"})
	common.DB.Create(&model.MarketSyncState{Symbol: "000002", Market: "cn", Name: "万科A"})
	now := time.Now()
	common.DB.Create(&model.News{
		Title: "关联多标的快讯", Source: "cls", SourceID: "n1",
		ContentHash: newsContentHash("关联多标的快讯", "a"),
		PublishTime: now, CollectTime: now,
		RelatedSymbols: `["600519","000002","999999"]`,
	})
	common.DB.Create(&model.News{
		Title: "无关联标的快讯", Source: "cls", SourceID: "n2",
		ContentHash: newsContentHash("无关联标的快讯", "b"),
		PublishTime: now.Add(-time.Minute), CollectTime: now,
		RelatedSymbols: "",
	})

	rows, err := NewNewsService().ListNews("", "", 10)
	if err != nil {
		t.Fatalf("查快讯失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条快讯: got %d", len(rows))
	}
	got := map[string]string{}
	for _, s := range rows[0].RelatedStocks {
		got[s.Symbol] = s.Name
		if s.Market != "cn" {
			t.Fatalf("A 股快讯关联标的市场应为 cn: %+v", s)
		}
	}
	if len(rows[0].RelatedStocks) != 3 {
		t.Fatalf("关联标的应保留全部 3 个（含无名称的）: %+v", rows[0].RelatedStocks)
	}
	if got["600519"] != "贵州茅台" {
		t.Fatalf("stocks 表名称未补上: %q", got["600519"])
	}
	if got["000002"] != "万科A" {
		t.Fatalf("宇宙字典回落未生效: %q", got["000002"])
	}
	if got["999999"] != "" {
		t.Fatalf("查不到名称的必须留空串而不是伪造/兜底文案: %q", got["999999"])
	}
	if len(rows[1].RelatedStocks) != 0 {
		t.Fatalf("无关联标的应为空数组: %+v", rows[1].RelatedStocks)
	}
}

// TestParseRelatedSymbolsTolerant 坏 JSON / 空串 / 空元素都不得让快讯查询炸掉。
func TestParseRelatedSymbolsTolerant(t *testing.T) {
	cases := map[string]int{
		"":                    0,
		"not-json":            0,
		"[]":                  0,
		`["600519"]`:          1,
		`["600519",""]`:       1,
		`["600519","000002"]`: 2,
		`{"a":1}`:             0,
	}
	for raw, want := range cases {
		if got := len(parseRelatedSymbols(raw)); got != want {
			t.Errorf("parseRelatedSymbols(%q)=%d want %d", raw, got, want)
		}
	}
}
