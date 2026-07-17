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
		s.seen[string(rune('a'+i%26))+string(rune(i))] = struct{}{}
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
	if !insertNews(n) {
		t.Fatal("首次入库应成功")
	}
	s.dedupeRegister("cls", "1", "标题甲事件", now)
	if !s.dedupeSeen("cls", "1", "标题甲事件") {
		t.Fatal("入库成功登记后应判重")
	}

	// 写库失败：删表让 insertNews 返回 false → 不登记 → 同条目仍不判重（下轮可重采）。
	if err := common.DB.Migrator().DropTable(&model.News{}); err != nil {
		t.Fatalf("drop news: %v", err)
	}
	if s.dedupeSeen("cls", "2", "标题乙事件") {
		t.Fatal("新条目不应判重")
	}
	n2 := &model.News{Title: "标题乙事件", Source: "cls", SourceID: "2",
		ContentHash: newsContentHash("标题乙事件", "y"), PublishTime: now, CollectTime: now}
	if insertNews(n2) {
		t.Fatal("表已删，入库应失败返回 false")
	}
	// 关键断言：写库失败未登记去重，同条目仍判为「未见过」，下轮重采不丢。
	if s.dedupeSeen("cls", "2", "标题乙事件") {
		t.Fatal("写库失败不应登记去重（否则该条永久丢失）")
	}
}
