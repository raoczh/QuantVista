package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// ---------- P1-5 反思记忆影子层 ----------

func setReflectionFlag(t *testing.T, v bool) {
	t.Helper()
	setupTestDB(t)
	if err := setting.SetLLMReflectionShadow(v); err != nil {
		t.Fatalf("切换反思影子开关失败: %v", err)
	}
	t.Cleanup(func() { _ = setting.SetLLMReflectionShadow(true) })
}

// cleanReflectionTables 共享内存库清场（seed 前后都清，防污染其他测试）。
func cleanReflectionTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Where("1=1").Delete(&model.RecommendationReflection{})
		common.DB.Where("1=1").Delete(&model.RecommendationLabel{})
		common.DB.Where("1=1").Delete(&model.Recommendation{})
		common.DB.Where("1=1").Delete(&model.LLMConfig{})
		common.DB.Where("username LIKE ?", "refl-%").Delete(&model.User{})
	}
	clean()
	t.Cleanup(clean)
}

// seedReflectionAdmin 反思生成用的系统默认 LLM（管理员配置指向假上游）。
func seedReflectionAdmin(t *testing.T, baseURL string) {
	t.Helper()
	common.EncryptionKey = "unit-test-key"
	admin := &model.User{Username: "refl-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	common.DB.Create(admin)
	cipher, _ := common.Encrypt("sk-refl")
	common.DB.Create(&model.LLMConfig{UserID: admin.ID, Name: "sys", Provider: "openai",
		BaseURL: baseURL, APIKeyCipher: cipher, Model: "m", IsDefault: true, MaxTokens: 8000})
}

// seedMaturedLabel 一条成熟标签（l2/next_open/真实推荐）+ 对应推荐条目。
func seedMaturedLabel(t *testing.T, userID int64, symbol, recType, strategy string, horizon int, net float64, hitTP, hitSL bool) model.RecommendationLabel {
	t.Helper()
	rec := model.Recommendation{UserID: userID, Symbol: symbol, Market: "cn", Name: "股" + symbol,
		Action: model.RecActionBuy, Summary: "动量突破放量", RefPrice: 10}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatalf("seed 推荐失败: %v", err)
	}
	l := model.RecommendationLabel{
		RecommendationID: rec.ID, HorizonDays: horizon, EntryMode: model.EntryModeNextOpen,
		UserID: userID, Symbol: symbol, Market: "cn", Type: recType, Action: model.RecActionBuy,
		SignalDate: "2026-06-01", NetReturnPct: net, AlphaPct: net - 1,
		HitTakeProfit: hitTP, HitStopLoss: hitSL,
		MaturityStatus: model.LabelMatured, Strategy: strategy, Regime: "neutral",
		EntryChg5dPct: 3.2, EntryTurnover: 5.5, EntryScore: 78,
		LabelVersion: labelVersion,
	}
	if err := common.DB.Create(&l).Error; err != nil {
		t.Fatalf("seed 标签失败: %v", err)
	}
	return l
}

// seedMaturedBulk 批量 seed 成熟标签凑门槛（非代表持有期，不进反思候选）。
func seedMaturedBulk(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedMaturedLabel(t, 1, "300"+padNum(i), model.RecTypeShortTerm, "momentum", 5, 1.0, false, false)
	}
}

func padNum(i int) string {
	s := "000" + itoa(i)
	return s[len(s)-3:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestReflectionOutcome 结算结局程序归类表驱动（LLM 不判输赢）。
func TestReflectionOutcome(t *testing.T) {
	cases := []struct {
		l    model.RecommendationLabel
		want string
	}{
		{model.RecommendationLabel{HitTakeProfit: true, NetReturnPct: 8}, "take_profit"},
		{model.RecommendationLabel{HitStopLoss: true, NetReturnPct: -6}, "stop_loss"},
		{model.RecommendationLabel{NetReturnPct: 2.5}, "win"},
		{model.RecommendationLabel{NetReturnPct: -1.2}, "loss"},
		{model.RecommendationLabel{NetReturnPct: 0}, "loss"},
	}
	for i, c := range cases {
		if got := reflectionOutcome(c.l); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

// TestReflectionBelowThresholdNoCall 门槛反例：成熟标签 <30 不发起任何 LLM 调用。
func TestReflectionBelowThresholdNoCall(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	seedReflectionAdmin(t, srv.URL)
	seedMaturedBulk(t, 10) // 仅 10 条 < 30

	n, err := GenerateRecommendationReflections(context.Background())
	if err != nil || n != 0 || calls != 0 {
		t.Fatalf("门槛不足应零调用零生成: n=%d calls=%d err=%v", n, calls, err)
	}
}

// TestReflectionFlagOff flag 关：生成与检索都停（已落库反思保留）。
func TestReflectionFlagOff(t *testing.T) {
	setReflectionFlag(t, false)
	cleanReflectionTables(t)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	seedReflectionAdmin(t, srv.URL)
	seedMaturedBulk(t, 35)

	if n, _ := GenerateRecommendationReflections(context.Background()); n != 0 || calls != 0 {
		t.Fatalf("flag 关应零调用: n=%d calls=%d", n, calls)
	}
	common.DB.Create(&model.RecommendationReflection{RecommendationID: 991, HorizonDays: 10,
		Symbol: "600100", Strategy: "momentum", RecType: model.RecTypeShortTerm,
		Outcome: "win", Lesson: "教训", AvailableFrom: time.Now().Add(-time.Hour), ReflectionVersion: reflectionVersion})
	if got := reflectionShadowJSON(0, model.RecTypeShortTerm, "momentum",
		[]candidate{{Symbol: "600100"}}); got != "" {
		t.Fatalf("flag 关检索应返回空: %s", got)
	}
}

// TestReflectionGenerateEndToEnd 假 LLM 端到端：门槛达标 → 只挑代表持有期未反思标签
// （≤5 条/轮）→ 落库（outcome/available_from/factor_digest 程序产出、lesson 截断）；
// idx 越界丢弃；重跑幂等（已反思行不再进候选）。
func TestReflectionGenerateEndToEnd(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	calls := 0
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		content := `{"reflections":[{"idx":0,"lesson":"方向判断正确；量能佐证成立。教训：同类放量突破可持有到位。"},` +
			`{"idx":1,"lesson":"止损被触发；追高论点失败。教训：5日涨幅超8%的动量票应等回踩。"},` +
			`{"idx":99,"lesson":"越界伪造关联"}]}`
		esc, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(esc) + `}}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}}`))
	}))
	defer srv.Close()
	seedReflectionAdmin(t, srv.URL)
	seedMaturedBulk(t, 30) // 凑门槛（horizon=5 非代表期，不进候选）

	// 两条代表持有期候选：短线 h=10 止盈、长线 h=20 止损。
	l1 := seedMaturedLabel(t, 1, "600100", model.RecTypeShortTerm, "momentum", 10, 9.5, true, false)
	l2 := seedMaturedLabel(t, 1, "600200", model.RecTypeLongTerm, "value", 20, -7.0, false, true)
	// updated_at 钉同一时刻：候选序落到 id ASC tiebreaker（l1 先建 → idx0）。
	// 不钉时两行跨毫秒创建会让 DESC 序漂移（与其他测试共跑曾因此教训错位 flaky）。
	common.DB.Model(&model.RecommendationLabel{}).Where("id IN ?", []int64{l1.ID, l2.ID}).
		Update("updated_at", time.Date(2026, 7, 1, 16, 0, 0, 0, time.Local))

	n, err := GenerateRecommendationReflections(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("应生成 2 条: n=%d err=%v", n, err)
	}
	if calls != 1 {
		t.Fatalf("批量应 1 次调用: %d", calls)
	}
	// 输入含推荐当时理由与结算结果（反思的对照材料）。
	if !strings.Contains(lastBody, "动量突破放量") || !strings.Contains(lastBody, "reason_then") {
		t.Fatalf("输入应含推荐当时理由: %s", lastBody[:min(400, len(lastBody))])
	}

	var rows []model.RecommendationReflection
	common.DB.Order("recommendation_id").Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("应落库 2 条（idx=99 越界丢弃）: %d", len(rows))
	}
	r1, r2 := rows[0], rows[1]
	if r1.RecommendationID != l1.RecommendationID || r1.HorizonDays != 10 ||
		r1.Outcome != "take_profit" || r1.RecType != model.RecTypeShortTerm {
		t.Fatalf("r1 字段不符: %+v", r1)
	}
	if r2.RecommendationID != l2.RecommendationID || r2.Outcome != "stop_loss" || r2.Strategy != "value" {
		t.Fatalf("r2 字段不符: %+v", r2)
	}
	if !strings.Contains(r1.Lesson, "放量突破") || r1.ReflectionVersion != reflectionVersion {
		t.Fatalf("lesson/版本不符: %+v", r1)
	}
	if r1.AvailableFrom.IsZero() || r1.AvailableFrom.Before(r1.LabelMaturedAt) {
		t.Fatalf("available_from 应为生成时刻（不早于标签成熟时刻）: %+v", r1)
	}
	var digest map[string]any
	if json.Unmarshal([]byte(r1.FactorDigest), &digest) != nil || digest["strategy"] != "momentum" {
		t.Fatalf("factor_digest 应为程序组装 JSON: %s", r1.FactorDigest)
	}

	// 幂等：重跑不再有候选（NOT EXISTS 排除已反思行），零调用零新增。
	calls = 0
	n, err = GenerateRecommendationReflections(context.Background())
	if err != nil || n != 0 || calls != 0 {
		t.Fatalf("重跑应幂等: n=%d calls=%d err=%v", n, calls, err)
	}
}

// TestReflectionAvailableFromFilter 防回放泄漏铁律：available_from 晚于检索时点的教训
// 不得返回——历史回放时未来结算结果不能进入过去的生成端。
func TestReflectionAvailableFromFilter(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	now := time.Now()
	common.DB.Create(&model.RecommendationReflection{RecommendationID: 1001, HorizonDays: 10,
		Symbol: "600100", Strategy: "momentum", RecType: model.RecTypeShortTerm,
		Outcome: "win", Lesson: "过去的教训", AvailableFrom: now.Add(-time.Hour), ReflectionVersion: reflectionVersion})
	common.DB.Create(&model.RecommendationReflection{RecommendationID: 1002, HorizonDays: 10,
		Symbol: "600100", Strategy: "momentum", RecType: model.RecTypeShortTerm,
		Outcome: "loss", Lesson: "未来的教训（不可见）", AvailableFrom: now.Add(time.Hour), ReflectionVersion: reflectionVersion})

	got := lookupReflections(now, 0, model.RecTypeShortTerm, "momentum", []string{"600100"})
	if len(got) != 1 || got[0].ID == 0 || !strings.Contains(got[0].Lesson, "过去的教训") {
		t.Fatalf("未来 available_from 必须被过滤: %+v", got)
	}
	// 回放时点更早：全部不可见。
	if got := lookupReflections(now.Add(-2*time.Hour), 0, model.RecTypeShortTerm, "momentum", []string{"600100"}); len(got) != 0 {
		t.Fatalf("回放时点早于全部教训生成时刻应为空: %+v", got)
	}
}

// TestReflectionLookupPriority 检索优先级：同标的命中排前、同策略补足、上限钳制、去重。
func TestReflectionLookupPriority(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	now := time.Now()
	mk := func(recID int64, symbol, strategy string) {
		common.DB.Create(&model.RecommendationReflection{RecommendationID: recID, HorizonDays: 10,
			Symbol: symbol, Strategy: strategy, RecType: model.RecTypeShortTerm,
			Outcome: "win", ReturnPct: 3, Lesson: "教训" + symbol, AvailableFrom: now.Add(-time.Hour),
			ReflectionVersion: reflectionVersion})
	}
	mk(2001, "600100", "pullback") // symbol 命中（策略不同也算——同标的教训语义更强）
	mk(2002, "600300", "momentum") // strategy 命中
	mk(2003, "600400", "momentum") // strategy 命中
	mk(2004, "600500", "other")    // 都不命中

	got := lookupReflections(now, 0, model.RecTypeShortTerm, "momentum", []string{"600100", "600900"})
	if len(got) != 3 {
		t.Fatalf("应命中 3 条（1 symbol + 2 strategy）: %+v", got)
	}
	if got[0].Symbol != "600100" || got[0].MatchedBy != "symbol" {
		t.Fatalf("symbol 命中应排前: %+v", got[0])
	}
	for _, m := range got[1:] {
		if m.MatchedBy != "strategy" {
			t.Fatalf("补足项应为 strategy 命中: %+v", m)
		}
	}
}

// TestReflectionShadowJSONNotMutatePicks 影子纪律：检索只落批次快照字段，picks 与
// prompt 构造零耦合——reflectionShadowJSON 是纯读函数，输出仅供落 batch.ReflectionJSON。
func TestReflectionShadowJSONNotMutatePicks(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	now := time.Now()
	common.DB.Create(&model.RecommendationReflection{RecommendationID: 3001, HorizonDays: 10,
		Symbol: "600100", Strategy: "momentum", RecType: model.RecTypeShortTerm,
		Outcome: "loss", ReturnPct: -5, Lesson: "追高失败教训", AvailableFrom: now.Add(-time.Hour),
		ReflectionVersion: reflectionVersion})

	cands := []candidate{{Symbol: "600100", Name: "甲", Price: 10, Score: 80}}
	before, _ := json.Marshal(cands)
	raw := reflectionShadowJSON(0, model.RecTypeShortTerm, "momentum", cands)
	after, _ := json.Marshal(cands)

	if string(before) != string(after) {
		t.Fatalf("影子检索不得改动候选: %s vs %s", before, after)
	}
	var snap reflectionShadowSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("快照应为合法 JSON: %v", err)
	}
	// P2-3：llm_layered_context 缺省开——快照版本 rf2 并携带分层元数据（Tier1=symbol
	// 命中、候选/被裁计数、Tier3 同策略结局统计）。
	if snap.Version != reflectionShadowVersion || len(snap.Matched) != 1 ||
		snap.Matched[0].Lesson != "追高失败教训" || !strings.Contains(snap.Note, "未注入") {
		t.Fatalf("快照形态不符: %+v", snap)
	}
	if snap.Layers == nil || snap.Layers.Tier1Count != 1 || snap.Layers.Tier2Count != 0 ||
		snap.Layers.CandidatesTotal != 1 || snap.Layers.TrimmedCount != 0 {
		t.Fatalf("分层元数据不符: %+v", snap.Layers)
	}
	if snap.Layers.Tier3Stats == nil || snap.Layers.Tier3Stats.Total != 1 ||
		snap.Layers.Tier3Stats.Losses != 1 || snap.Layers.Tier3Stats.AvgReturnPct != -5 {
		t.Fatalf("Tier3 结局统计不符: %+v", snap.Layers.Tier3Stats)
	}
	// 无匹配（标的与策略都不命中）：空串（批次列保持空，前端零噪声）。
	if got := reflectionShadowJSON(0, model.RecTypeShortTerm, "pullback", []candidate{{Symbol: "000999"}}); got != "" {
		t.Fatalf("无匹配应返回空串: %s", got)
	}
}

// TestReflectionUserIsolation 用户隔离铁律（审查修复批）：影子检索只返回本用户的
// 教训——反思行携带来源用户的标的/收益结局/教训文本，跨用户返回即数据泄漏。
func TestReflectionUserIsolation(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	now := time.Now()
	common.DB.Create(&model.RecommendationReflection{RecommendationID: 5001, HorizonDays: 10,
		UserID: 11, Symbol: "600100", Strategy: "momentum", RecType: model.RecTypeShortTerm,
		Outcome: "loss", ReturnPct: -8, Lesson: "用户A的教训", AvailableFrom: now.Add(-time.Hour),
		ReflectionVersion: reflectionVersion})

	// 用户 B（22）检索同标的/同策略：一条都不可见。
	if got := lookupReflections(now, 22, model.RecTypeShortTerm, "momentum", []string{"600100"}); len(got) != 0 {
		t.Fatalf("跨用户教训不得返回: %+v", got)
	}
	if got := reflectionShadowJSON(22, model.RecTypeShortTerm, "momentum", []candidate{{Symbol: "600100"}}); got != "" {
		t.Fatalf("跨用户影子快照应为空: %s", got)
	}
	// 本人可见。
	if got := lookupReflections(now, 11, model.RecTypeShortTerm, "momentum", []string{"600100"}); len(got) != 1 {
		t.Fatalf("本人教训应可见: %+v", got)
	}
}

// TestReflectionOrphanLabelsNotBlocking 孤儿标签不阻塞（审查修复批）：推荐条目已删的
// 标签在 SQL 侧排除——「最新 N 条全是孤儿」时更早的有效候选仍能被选中，不会每轮
// 重复选中孤儿导致永久饿死。
func TestReflectionOrphanLabelsNotBlocking(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)

	// 先落 1 条有效候选（较早），再落 6 条孤儿（较新：标签在、推荐行删除）。
	valid := seedMaturedLabel(t, 1, "600700", model.RecTypeShortTerm, "momentum", 10, 4.2, false, false)
	common.DB.Model(&model.RecommendationLabel{}).Where("id = ?", valid.ID).
		Update("updated_at", time.Now().Add(-time.Hour))
	for i := 0; i < 6; i++ {
		l := seedMaturedLabel(t, 1, fmt.Sprintf("6008%02d", i), model.RecTypeShortTerm, "momentum", 10, 1, false, false)
		common.DB.Delete(&model.Recommendation{}, l.RecommendationID)
	}

	cands, err := loadReflectionCandidates(5)
	if err != nil {
		t.Fatalf("loadReflectionCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].label.ID != valid.ID {
		t.Fatalf("孤儿标签应在 SQL 侧排除，仅返回有效候选: %d 条", len(cands))
	}
}
