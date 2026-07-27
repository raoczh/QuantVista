package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
)

// P1-1 推荐 coverage/越池诊断测试（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.2 P1-1、
// §4.5.5 蓝图 D diagnostics）：程序化计数与机读错误码、manifest coverage 真实填充、
// 量化 fallback 保留 screen score。诊断是观测不是门控——picks/rejected 行为与
// 改造前一致由 recommendation_test.go 既有用例锁定，本文件锁诊断本身。

// TestParseAndFilterPicksCoverageDiag 混合场景手工验算：2 有效 + 1 越池 + 1 乱码 +
// 1 空白 + 2 重复 + 1 超量截断，各计数/样本/coverage/error_codes 逐项核对。
func TestParseAndFilterPicksCoverageDiag(t *testing.T) {
	pool := map[string]candidate{
		"600000": {Symbol: "600000", Name: "浦发银行", Price: 8.5},
		"000001": {Symbol: "000001", Name: "平安银行", Price: 11.2},
		"600036": {Symbol: "600036", Name: "招商银行", Price: 35},
	}
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70},
		{"symbol":"999999","action":"buy","confidence":90},
		{"symbol":"AAPL","action":"buy","confidence":80},
		{"symbol":"600000","action":"watch","confidence":40},
		{"symbol":"","action":"buy","confidence":50},
		{"symbol":"000001","action":"watch","confidence":55},
		{"symbol":"600036","action":"buy","confidence":60},
		{"symbol":"999999","action":"buy","confidence":85}
	]}`
	picks, _, diag, err := parseAndFilterPicks(content, pool, 2)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	// 行为不变式：保留前 maxCount 个池内首现标的（与旧 break 语义逐条一致）。
	if len(picks) != 2 || picks[0].Symbol != "600000" || picks[1].Symbol != "000001" {
		t.Fatalf("picks 行为不得因诊断改变: %+v", picks)
	}
	if diag == nil {
		t.Fatalf("有结构输出时诊断不得为 nil")
	}
	// 计数手工验算：input=8；covered=2（600000/000001）；truncated=1（600036 超量）；
	// out_of_pool=1（999999 首现）；unknown=2（AAPL+空白）；duplicate=2（600000/999999 二现）。
	if diag.InputCount != 8 || diag.CoveredCount != 2 || diag.TruncatedCount != 1 ||
		diag.OutOfPoolCount != 1 || diag.UnknownCount != 2 || diag.DuplicateCount != 2 {
		t.Fatalf("计数与手工验算不符: %+v", diag)
	}
	// 不变式：input = covered + truncated + duplicate + unknown + out_of_pool。
	if diag.InputCount != diag.CoveredCount+diag.TruncatedCount+diag.DuplicateCount+diag.UnknownCount+diag.OutOfPoolCount {
		t.Fatalf("条目归类不变式被破坏: %+v", diag)
	}
	// unique = 8-2 = 6；coverage = (2+1)/6 = 0.5。
	if diag.UniqueCount != 6 || diag.Coverage != 0.5 {
		t.Fatalf("unique/coverage 验算不符: unique=%d coverage=%v", diag.UniqueCount, diag.Coverage)
	}
	// 样本数组：空白不进样本；重复收 600000 与 999999。
	if len(diag.UnknownSymbols) != 1 || diag.UnknownSymbols[0] != "AAPL" {
		t.Fatalf("unknown 样本应仅 AAPL（空白不进数组）: %v", diag.UnknownSymbols)
	}
	if len(diag.OutOfPoolSymbols) != 1 || diag.OutOfPoolSymbols[0] != "999999" {
		t.Fatalf("out_of_pool 样本: %v", diag.OutOfPoolSymbols)
	}
	if len(diag.DuplicateSymbols) != 2 {
		t.Fatalf("duplicate 样本应 2 个: %v", diag.DuplicateSymbols)
	}
	// 机读错误码四类齐全。
	for _, code := range []string{recDiagOutOfPool, recDiagUnknown, recDiagDuplicate, recDiagOverflow} {
		found := false
		for _, c := range diag.ErrorCodes {
			if c == code {
				found = true
			}
		}
		if !found {
			t.Fatalf("error_codes 缺 %s: %v", code, diag.ErrorCodes)
		}
	}
}

// TestParseAndFilterPicksCoverageClean 全部合法时诊断为「零违规」形态：coverage=1、
// 各缺陷计数 0、error_codes 空——正常批次的 manifest 不应带出告警噪声。
func TestParseAndFilterPicksCoverageClean(t *testing.T) {
	pool := map[string]candidate{
		"600000": {Symbol: "600000", Name: "浦发银行", Price: 8.5},
		"000001": {Symbol: "000001", Name: "平安银行", Price: 11.2},
	}
	content := `{"picks":[
		{"symbol":"600000","action":"buy","confidence":70},
		{"symbol":"000001","action":"watch","confidence":55}
	]}`
	_, _, diag, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if diag.InputCount != 2 || diag.CoveredCount != 2 || diag.UniqueCount != 2 || diag.Coverage != 1 {
		t.Fatalf("全合法计数: %+v", diag)
	}
	if len(diag.ErrorCodes) != 0 || diag.TruncatedCount != 0 || diag.UnknownCount != 0 {
		t.Fatalf("全合法不应带错误码: %+v", diag)
	}
}

// TestParseAndFilterPicksCoverageEmptyPicks 显式空拒选：coverage=1（无输出即无违规），
// 不误报成「全越池」。
func TestParseAndFilterPicksCoverageEmptyPicks(t *testing.T) {
	pool := testPool()
	_, _, diag, err := parseAndFilterPicks(`{"picks":[]}`, pool, 5)
	if err != nil {
		t.Fatalf("空拒选合法: %v", err)
	}
	if diag == nil || diag.InputCount != 0 || diag.Coverage != 1 || len(diag.ErrorCodes) != 0 {
		t.Fatalf("空拒选诊断应为零违规形态: %+v", diag)
	}
}

// TestParseAndFilterPicksCoverageAllInvalid 全越池返回 error（触发 repair）时**必须**
// 同时带诊断——repair 打满仍失败的降级批次靠它归因「为什么无效」；错误文案带分类计数
// 帮助 repair 反馈更精准。
func TestParseAndFilterPicksCoverageAllInvalid(t *testing.T) {
	pool := testPool()
	content := `{"picks":[{"symbol":"999999","action":"buy","confidence":80},{"symbol":"贵州茅台","action":"buy","confidence":80}]}`
	_, _, diag, err := parseAndFilterPicks(content, pool, 5)
	if err == nil {
		t.Fatalf("全越池应返回错误")
	}
	if diag == nil || diag.OutOfPoolCount != 1 || diag.UnknownCount != 1 || diag.CoveredCount != 0 {
		t.Fatalf("error 路径诊断: %+v", diag)
	}
	if !strings.Contains(err.Error(), "池外 1") || !strings.Contains(err.Error(), "无法识别 1") {
		t.Fatalf("错误文案应含分类计数: %v", err)
	}
	// JSON 解析失败/缺 picks 字段：无结构可诊断，diag 必须为 nil（不造假诊断）。
	if _, _, d, err := parseAndFilterPicks("不是 JSON", pool, 5); err == nil || d != nil {
		t.Fatalf("坏 JSON 应 err 且 diag=nil: %v %+v", err, d)
	}
	if _, _, d, err := parseAndFilterPicks(`{"rejected":[]}`, pool, 5); err == nil || d != nil {
		t.Fatalf("缺 picks 应 err 且 diag=nil: %v %+v", err, d)
	}
}

// TestParseAndFilterPicksRejectedDropped 落选理由的越池/重复/空代码/空理由剥除计数；
// 「已入选不算落选」是正常语义剥除不计。
func TestParseAndFilterPicksRejectedDropped(t *testing.T) {
	pool := testPool()
	content := `{"picks":[{"symbol":"600000","action":"buy","confidence":70}],"rejected":[
		{"symbol":"000001","reason":"量价背离"},
		{"symbol":"999999","reason":"池外"},
		{"symbol":"000001","reason":"重复"},
		{"symbol":"","reason":"空代码"},
		{"symbol":"600000","reason":"已入选（不计 dropped）"}
	]}`
	_, rejected, diag, err := parseAndFilterPicks(content, pool, 5)
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Symbol != "000001" {
		t.Fatalf("rejected 行为不得因诊断改变: %+v", rejected)
	}
	if diag.RejectedDropped != 3 {
		t.Fatalf("rejected_dropped 应为 3（池外+重复+空代码；已入选不计）: %d", diag.RejectedDropped)
	}
}

// TestMarkCoveragePromptTrimmed 输入侧裁剪声明：nil 安全、kept<=llm 不标、kept>llm 标注。
func TestMarkCoveragePromptTrimmed(t *testing.T) {
	markCoveragePromptTrimmed(nil, 40, 10) // nil 不 panic、不造空诊断
	d := &RecCoverageDiag{}
	markCoveragePromptTrimmed(d, 10, 10)
	if d.PromptTrimmed {
		t.Fatalf("kept==llm 不应标 trimmed")
	}
	markCoveragePromptTrimmed(d, 42, 10)
	if !d.PromptTrimmed || !strings.Contains(d.PromptTrimmedNote, "42") || !strings.Contains(d.PromptTrimmedNote, "10") {
		t.Fatalf("kept>llm 应标 trimmed 并带说明: %+v", d)
	}
}

// TestRecCoverageManifestEndToEnd callWithRepair 层端到端：假 LLM 返回含越池标的的
// 有效输出 → run.Coverage 挂载程序化诊断 → manifest JSON 真实带出 coverage 字段
// （P0-2 起恒空预留，本批起填充真实值）；非推荐 run 的 manifest 不含 coverage。
func TestRecCoverageManifestEndToEnd(t *testing.T) {
	setupTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{\"picks\":[{\"symbol\":\"600001\",\"action\":\"buy\",\"confidence\":70,\"reason\":[\"r\"],\"risks\":[\"k\"],\"evidence\":[\"e\"]},{\"symbol\":\"999999\",\"action\":\"buy\",\"confidence\":90}],\"rejected\":[]}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	svc := &RecommendationService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m"}
	pool := map[string]candidate{"600001": {Symbol: "600001", Name: "甲", Price: 10}}
	run := newLLMRun("t-cov", "", "recommendation", "recommendation.v1", "p12")
	picks, _, _, _, err := svc.callWithRepair(context.Background(), 51, run, cfg, "sk", true,
		[]chatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}, pool, 3)
	if err != nil || len(picks) != 1 {
		t.Fatalf("应成功保留 1 条: err=%v picks=%d", err, len(picks))
	}
	if run.Coverage == nil || run.Coverage.CoveredCount != 1 || run.Coverage.OutOfPoolCount != 1 || run.Coverage.Coverage != 0.5 {
		t.Fatalf("run.Coverage 应挂载程序化诊断: %+v", run.Coverage)
	}
	// 输入侧裁剪声明（runGeneration 层语义在此模拟）。
	markCoveragePromptTrimmed(run.Coverage, 42, 10)

	out := marshalLLMRunManifests(cfg, runEntry(run, true))
	if out == "" {
		t.Fatalf("manifest 不应为空")
	}
	var ms []LLMRunManifest
	if err := json.Unmarshal([]byte(out), &ms); err != nil || len(ms) != 1 {
		t.Fatalf("manifest 解析: %v %s", err, out)
	}
	cov := ms[0].Coverage
	if cov == nil || cov.InputCount != 2 || cov.CoveredCount != 1 || cov.OutOfPoolCount != 1 ||
		cov.Coverage != 0.5 || !cov.PromptTrimmed {
		t.Fatalf("manifest coverage 应带真实诊断: %+v", cov)
	}
	if len(cov.OutOfPoolSymbols) != 1 || cov.OutOfPoolSymbols[0] != "999999" {
		t.Fatalf("manifest 应带越池样本: %v", cov.OutOfPoolSymbols)
	}
	if !strings.Contains(out, recDiagOutOfPool) {
		t.Fatalf("manifest JSON 应含机读错误码: %s", out)
	}

	// 反例：非推荐 run（无 Coverage 来源）的 manifest 不得带 coverage 键——
	// P0-2 明令不得在无程序化计数的模块伪造。
	other := newLLMRun("t-cov", "", "analysis", "analysis.v1", "p19")
	other.Attempts = 1
	if s := marshalLLMRunManifests(cfg, runEntry(other, true)); strings.Contains(s, "coverage") {
		t.Fatalf("非推荐 run 不应带 coverage: %s", s)
	}
}

// TestRecCoverageRepairFailKeepsLastDiag 首轮全越池（有结构诊断）→ repair 轮返回非 JSON
// （无结构，diag=nil）→ 打满降级：run.Coverage 必须保留**最后一轮有结构输出**的诊断，
// 不被 nil 覆盖——降级批次的 manifest 才能归因「模型全在越池选股」。
func TestRecCoverageRepairFailKeepsLastDiag(t *testing.T) {
	setupTestDB(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content := `{\"picks\":[{\"symbol\":\"999999\",\"action\":\"buy\",\"confidence\":80}]}`
		if calls >= 2 {
			content = `完全不是 JSON 的输出`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	svc := &RecommendationService{}
	cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "m"}
	pool := map[string]candidate{"600001": {Symbol: "600001", Name: "甲", Price: 10}}
	run := newLLMRun("t-keep", "", "recommendation", "recommendation.v1", "p12")
	picks, _, _, _, err := svc.callWithRepair(context.Background(), 52, run, cfg, "sk", true,
		[]chatMessage{{Role: "user", Content: "u"}}, pool, 3)
	if err != nil || len(picks) != 0 {
		t.Fatalf("打满应走降级返回（nil picks, nil err）: err=%v picks=%d", err, len(picks))
	}
	if calls != 1+moduleRepairAttempts("recommendation") {
		t.Fatalf("应打满 repair: calls=%d", calls)
	}
	if run.Coverage == nil || run.Coverage.OutOfPoolCount != 1 {
		t.Fatalf("run.Coverage 应保留首轮越池诊断（不被坏 JSON 轮清空）: %+v", run.Coverage)
	}
}

// TestQuantFallbackKeepsScreenScore 量化 fallback 保留 screen score（P1-1 门槛）：
// picks 按量化排名（Rank）升序、每条 Reason/Evidence 以文本与机读双形态携带量化分与
// 排名；DegradedSource 经 normalizePick 不被剥除（构造路径先设值语义）。结构化的
// QuantScore/QuantRank 由 runGeneration 信任层回填（fallback 替换发生在信任层之前，
// 同一循环生效——见 runGeneration 注释）。
func TestQuantFallbackKeepsScreenScore(t *testing.T) {
	cands := []candidate{
		{Symbol: "000002", Name: "乙", Price: 20, Rank: 2, Score: 71.5, ChangePct: 1.2},
		{Symbol: "600001", Name: "甲", Price: 10, Rank: 1, Score: 83.4, ChangePct: 2.5},
		{Symbol: "000003", Name: "丙", Price: 30, Rank: 3, Score: 65.0, ChangePct: -0.8},
	}
	picks := buildQuantFallbackPicks(model.RecTypeShortTerm, cands, 2)
	if len(picks) != 2 {
		t.Fatalf("应取前 2 名: %d", len(picks))
	}
	// 按 screen score 排名序（Rank 升序），非入参顺序。
	if picks[0].Symbol != "600001" || picks[1].Symbol != "000002" {
		t.Fatalf("fallback 应按量化排名序: %s, %s", picks[0].Symbol, picks[1].Symbol)
	}
	for i, p := range picks {
		if p.DegradedSource != "quant_fallback" {
			t.Fatalf("DegradedSource 不得被 normalizePick 剥除: %+v", p)
		}
		if len(p.Reason) == 0 || !strings.Contains(p.Reason[0], "量化综合分") {
			t.Fatalf("Reason 应含量化分说明: %+v", p.Reason)
		}
		found := false
		for _, e := range p.Evidence {
			if strings.Contains(e, "score=") && strings.Contains(e, "rank=") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Evidence 应含 score=/rank= 机读形态: %+v", p.Evidence)
		}
		_ = i
	}
	// 首名分数如实透传（83.4 而非重算/丢失）。
	if !strings.Contains(picks[0].Reason[0], "83.4") || !strings.Contains(picks[0].Evidence[0], "score=83.4 rank=1") {
		t.Fatalf("screen score 数值应逐字保留: %+v", picks[0])
	}
}
