package setting

import "testing"

// TestClampNewsInterval 采集间隔解析：缺失/非法回默认 5，越界钳到 [1,120]。
func TestClampNewsInterval(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", NewsIntervalDefault},
		{"abc", NewsIntervalDefault},
		{"0", NewsIntervalDefault},
		{"-3", NewsIntervalDefault},
		{"1", 1},
		{"5", 5},
		{"120", 120},
		{"121", 120},
		{"99999", 120},
	}
	for _, c := range cases {
		if got := clampNewsInterval(c.raw); got != c.want {
			t.Fatalf("clampNewsInterval(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestApplyNewsOptions news_auto_llm 语义必须是 != "false"：key 缺失（老库升级）默认开，
// 显式 false 才关；间隔走 clamp。
func TestApplyNewsOptions(t *testing.T) {
	apply(map[string]string{}) // 老库：两 key 都缺失
	if !NewsAutoLLM() {
		t.Fatal("news_auto_llm 缺失时应默认开启（老库升级不能静默关闭自动 LLM）")
	}
	if NewsCollectIntervalMin() != NewsIntervalDefault {
		t.Fatalf("间隔缺失应回默认 %d，得到 %d", NewsIntervalDefault, NewsCollectIntervalMin())
	}

	apply(map[string]string{"news_auto_llm": "false", "news_collect_interval_min": "30"})
	if NewsAutoLLM() {
		t.Fatal("显式 false 应关闭自动 LLM")
	}
	if NewsCollectIntervalMin() != 30 {
		t.Fatalf("间隔应为 30，得到 %d", NewsCollectIntervalMin())
	}

	// 恢复默认，防止内存状态污染同包其他测试。
	apply(map[string]string{})
}
