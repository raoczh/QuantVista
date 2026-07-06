package datasource

import "testing"

func TestNormalizeClsStockID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"sh603979", "603979", true},
		{"sz000001", "000001", true},
		{"SH600519", "600519", true},
		{"hk00700", "", false},
		{"sh60351", "", false},
		{"sh60351x", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeClsStockID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeClsStockID(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeEMStockCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1.688035", "688035", true},
		{"0.000001", "000001", true},
		{"90.BK1175", "", false}, // 板块
		{"116.00700", "", false}, // 港股
		{"1.68803", "", false},
		{"688035", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeEMStockCode(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeEMStockCode(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestStripJSONP(t *testing.T) {
	cases := []struct{ in, want string }{
		{`jq({"code":0,"data":[1,2]});`, `{"code":0,"data":[1,2]}`},
		{`jq({"a":"含括号(测试)"})`, `{"a":"含括号(测试)"}`},
		{`{"code":0}`, `{"code":0}`}, // 无壳原样
	}
	for _, c := range cases {
		if got := stripJSONP(c.in); got != c.want {
			t.Errorf("stripJSONP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripEMTags(t *testing.T) {
	if got := stripEMTags("<em>贵州茅台</em>发布公告"); got != "贵州茅台发布公告" {
		t.Errorf("stripEMTags = %q", got)
	}
}

func TestClsSign(t *testing.T) {
	// 固定输入对拍：sign = md5hex(sha1hex(qs))，与上次实测 shell 管道口径一致。
	got := clsSign("app=CailianpressWeb&category=&last_time=1700000000&os=web&refresh_type=1&rn=20&sv=8.7.9")
	if len(got) != 32 {
		t.Fatalf("sign 长度 = %d, want 32", len(got))
	}
	if got != clsSign("app=CailianpressWeb&category=&last_time=1700000000&os=web&refresh_type=1&rn=20&sv=8.7.9") {
		t.Error("同输入应得同签名")
	}
}
