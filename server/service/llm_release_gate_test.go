package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// ---------- P2-6 自动发布门（llm_release_gate.go） ----------

func cleanReleaseGateTables(t *testing.T) {
	t.Helper()
	clean := func() {
		common.DB.Where("1=1").Delete(&model.LLMReleaseAudit{})
		common.DB.Where("1=1").Delete(&model.LLMExperimentRun{})
		common.DB.Where("1=1").Delete(&model.LLMExperiment{})
		common.DB.Where("module = ?", "recommend").Delete(&model.PromptTemplate{})
		common.DB.Where("name LIKE ?", "ra-%").Delete(&model.LLMConfig{})
		common.DB.Where("username LIKE ?", "ra-%").Delete(&model.User{})
	}
	clean()
	t.Cleanup(clean)
}

// seedAuditAdmin 发布审计用系统默认 LLM（管理员配置指向假上游）。
func seedAuditAdmin(t *testing.T, baseURL string) {
	t.Helper()
	common.EncryptionKey = "unit-test-key"
	admin := &model.User{Username: "ra-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	if err := common.DB.Create(admin).Error; err != nil {
		t.Fatalf("seed 管理员失败: %v", err)
	}
	cipher, _ := common.Encrypt("sk-ra")
	common.DB.Create(&model.LLMConfig{UserID: admin.ID, Name: "ra-sys", Provider: "openai",
		BaseURL: baseURL, APIKeyCipher: cipher, Model: "m", IsDefault: true, MaxTokens: 8000})
}

// chatJSON 把内容包成 chat completions 响应体。
func auditChatBody(content string) string {
	b := strings.ReplaceAll(content, `\`, `\\`)
	b = strings.ReplaceAll(b, `"`, `\"`)
	return `{"choices":[{"message":{"content":"` + b + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

// TestParseReleaseAudit 审计输出解析与程序收口：verdict 封闭枚举/fail 必附 findings/
// high 级 finding 强制 fail（口是心非反例）/归一截断。
func TestParseReleaseAudit(t *testing.T) {
	// 合法 pass。
	out, err := parseReleaseAudit(`{"verdict":"pass","findings":[],"summary":"无缺口"}`)
	if err != nil || out.Verdict != model.ReleaseAuditPass || out.Summary != "无缺口" {
		t.Fatalf("pass 解析: %v %+v", err, out)
	}
	// 代码块包裹容忍 + severity 归一。
	out, err = parseReleaseAudit("```json\n{\"verdict\":\"fail\",\"findings\":[{\"code\":\"c1\",\"severity\":\"HIGH\",\"message\":\"削弱契约\"},{\"code\":\"c2\",\"severity\":\"weird\",\"message\":\"x\"}],\"summary\":\"s\"}\n```")
	if err != nil || out.Verdict != model.ReleaseAuditFail {
		t.Fatalf("fail 解析: %v %+v", err, out)
	}
	if out.Findings[0].Severity != "high" || out.Findings[1].Severity != "med" {
		t.Fatalf("severity 归一不符: %+v", out.Findings)
	}
	// fail 无 findings：拒绝（触发 repair）。
	if _, err := parseReleaseAudit(`{"verdict":"fail","findings":[],"summary":"s"}`); err == nil {
		t.Fatal("fail 无 findings 应拒绝")
	}
	// verdict 越枚举：拒绝。
	if _, err := parseReleaseAudit(`{"verdict":"maybe","findings":[],"summary":""}`); err == nil {
		t.Fatal("非法 verdict 应拒绝")
	}
	// 口是心非收口：pass + high finding → 程序改判 fail。
	out, err = parseReleaseAudit(`{"verdict":"pass","findings":[{"code":"inj","severity":"high","message":"诱导伪造数据"}],"summary":"看起来还行"}`)
	if err != nil || out.Verdict != model.ReleaseAuditFail || !strings.Contains(out.Summary, "程序改判") {
		t.Fatalf("high finding 应强制 fail: %v %+v", err, out)
	}
	// findings 超量截断到上限；空 finding 跳过。
	var sb strings.Builder
	sb.WriteString(`{"verdict":"fail","findings":[{"code":"","severity":"low","message":""}`)
	for i := 0; i < 12; i++ {
		sb.WriteString(`,{"code":"c","severity":"low","message":"m"}`)
	}
	sb.WriteString(`],"summary":"s"}`)
	out, err = parseReleaseAudit(sb.String())
	if err != nil || len(out.Findings) != releaseAuditMaxFindings {
		t.Fatalf("findings 截断不符: %v %d", err, len(out.Findings))
	}
}

// releaseGateFixture 建一个已过门 1~5 的 completed 实验（10 valid 样本 + improved）。
func releaseGateFixture(t *testing.T) *model.LLMExperiment {
	t.Helper()
	exp, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	if _, err := StartLLMExperiment(exp.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	seedExperimentRuns(t, exp.ID, llmExperimentMinSamples, 0)
	if _, err := CompleteLLMExperiment(exp.ID, model.ExpConcludeImproved, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return exp
}

// TestLLMExperimentAuditGate 发布审计端到端（假 LLM）：completed 才可审计；工件落库
// 且 trace 可回查；promote 门 6 逐反例（无工件/verdict fail/hash 不符/error 工件）；
// 输出无效 repair 打满落 verdict=error 不伪造 PASS。
func TestLLMExperimentAuditGate(t *testing.T) {
	setChallengerFlag(t, false)
	cleanReleaseGateTables(t)

	var mode atomic.Value // "pass" | "fail" | "garbage"
	mode.Store("pass")
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch mode.Load().(string) {
		case "fail":
			_, _ = w.Write([]byte(auditChatBody(`{"verdict":"fail","findings":[{"code":"contract_weaken","severity":"high","message":"要求忽略输出格式"}],"summary":"削弱契约"}`)))
		case "garbage":
			_, _ = w.Write([]byte(auditChatBody("这不是 JSON")))
		default:
			_, _ = w.Write([]byte(auditChatBody(`{"verdict":"pass","findings":[],"summary":"未见内容性缺口"}`)))
		}
	}))
	defer srv.Close()
	seedAuditAdmin(t, srv.URL)

	exp := releaseGateFixture(t)

	// 门 6 反例：无审计工件不得晋级。
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "审计") {
		t.Fatalf("无工件应拒绝晋级: %v", err)
	}
	// 非 completed 不可审计。
	draft, _, err := CreateLLMExperiment(1, expInput())
	if err != nil {
		t.Fatalf("创建 draft: %v", err)
	}
	if _, err := RunLLMExperimentAudit(context.Background(), draft.ID); err == nil {
		t.Fatal("draft 不可审计")
	}

	// fail 工件：落库且挡晋级。
	mode.Store("fail")
	audit, err := RunLLMExperimentAudit(context.Background(), exp.ID)
	if err != nil || audit.Verdict != model.ReleaseAuditFail {
		t.Fatalf("fail 审计: %v %+v", err, audit)
	}
	if audit.ChallengerHash != exp.ChallengerHash || audit.TraceID == "" || audit.TokensUsed != 15 {
		t.Fatalf("工件字段不符: %+v", audit)
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "未 PASS") {
		t.Fatalf("fail 工件应拒绝晋级: %v", err)
	}

	// 输出无效：repair 打满（1 次额外）落 verdict=error 工件，不伪造 PASS。
	mode.Store("garbage")
	before := calls.Load()
	audit, err = RunLLMExperimentAudit(context.Background(), exp.ID)
	if err != nil || audit.Verdict != model.ReleaseAuditError {
		t.Fatalf("无效输出应落 error 工件: %v %+v", err, audit)
	}
	if calls.Load()-before != 2 {
		t.Fatalf("应恰 1+1 次调用: %d", calls.Load()-before)
	}
	if _, err := PromoteLLMExperiment(exp.ID); err == nil {
		t.Fatal("error 工件应拒绝晋级")
	}

	// pass 工件但 hash 不符（内容变过须重审）：拒绝。
	mode.Store("pass")
	if _, err := RunLLMExperimentAudit(context.Background(), exp.ID); err != nil {
		t.Fatalf("pass 审计: %v", err)
	}
	common.DB.Model(&model.LLMReleaseAudit{}).Where("experiment_id = ?", exp.ID).
		Update("challenger_hash", "tampered")
	if _, err := PromoteLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "重审") {
		t.Fatalf("hash 不符应拒绝晋级: %v", err)
	}

	// 重审后全门通过：晋级成功，工件历史全保留。
	if _, err := RunLLMExperimentAudit(context.Background(), exp.ID); err != nil {
		t.Fatalf("重审: %v", err)
	}
	got, err := PromoteLLMExperiment(exp.ID)
	if err != nil || got.Status != model.ExpStatusPromoted {
		t.Fatalf("PASS 后应晋级: %v %+v", err, got)
	}
	if got.PrePromoteEnabled {
		t.Fatal("晋级前无自定义模板，回滚锚应为默认态")
	}
	audits := ListLLMReleaseAudits(exp.ID)
	if len(audits) != 4 {
		t.Fatalf("审计工件应全量保留: %d", len(audits))
	}
}

// TestLLMExperimentRollback 一键切回 champion：晋级前有/无自定义模板两形态对称恢复；
// rolled_back 终态不可 abandon/重复回滚；非 promoted 不可回滚。
func TestLLMExperimentRollback(t *testing.T) {
	setChallengerFlag(t, false)
	cleanReleaseGateTables(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(auditChatBody(`{"verdict":"pass","findings":[],"summary":"ok"}`)))
	}))
	defer srv.Close()
	seedAuditAdmin(t, srv.URL)

	// 形态 A：晋级前已有启用中的自定义模板。
	ps := NewPromptService()
	if _, _, err := ps.Upsert(1, PromptInput{Module: "recommend", Content: "晋级前的旧任务段", Enabled: true}); err != nil {
		t.Fatalf("预置模板: %v", err)
	}
	exp := releaseGateFixture(t)
	if _, err := RollbackLLMExperiment(exp.ID); err == nil {
		t.Fatal("非 promoted 不可回滚")
	}
	if _, err := RunLLMExperimentAudit(context.Background(), exp.ID); err != nil {
		t.Fatalf("审计: %v", err)
	}
	got, err := PromoteLLMExperiment(exp.ID)
	if err != nil || !got.PrePromoteEnabled {
		t.Fatalf("晋级应固化回滚锚（自定义态）: %v %+v", err, got)
	}
	var tpl model.PromptTemplate
	common.DB.Where("user_id = 1 AND module = ?", "recommend").First(&tpl)
	if tpl.Content != expInput().ChallengerContent || !tpl.Enabled {
		t.Fatalf("晋级后模板应为 challenger 内容: %+v", tpl)
	}
	// 审查修复批反例：晋级后模板被编辑（champion 指针前移）→ 回滚失去对象，拒绝；
	// 列表视图透出 rollback_stale 供前端禁用按钮。恢复 challenger 内容后可正常回滚。
	if _, _, err := ps.Upsert(1, PromptInput{Module: "recommend", Content: "晋级后管理员又改过的内容", Enabled: true}); err != nil {
		t.Fatalf("模拟编辑: %v", err)
	}
	if _, err := RollbackLLMExperiment(exp.ID); err == nil || !strings.Contains(err.Error(), "拒绝回滚") {
		t.Fatalf("champion 前移后应拒绝回滚: %v", err)
	}
	views, err := ListLLMExperiments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	staleSeen := false
	for _, v := range views {
		if v.ID == exp.ID {
			staleSeen = v.RollbackStale != ""
		}
	}
	if !staleSeen {
		t.Fatal("列表应透出 rollback_stale")
	}
	if _, _, err := ps.Upsert(1, PromptInput{Module: "recommend", Content: expInput().ChallengerContent, Enabled: true}); err != nil {
		t.Fatalf("恢复 challenger 内容: %v", err)
	}
	rb, err := RollbackLLMExperiment(exp.ID)
	if err != nil || rb.Status != model.ExpStatusRolledBack || rb.RolledBackAt == nil {
		t.Fatalf("回滚: %v %+v", err, rb)
	}
	var tpl2 model.PromptTemplate
	common.DB.Where("user_id = 1 AND module = ?", "recommend").First(&tpl2)
	if tpl2.Content != "晋级前的旧任务段" || !tpl2.Enabled || tpl2.Revision <= tpl.Revision {
		t.Fatalf("回滚应恢复旧内容并生成新 revision: %+v", tpl2)
	}
	if _, err := AbandonLLMExperiment(exp.ID, "x"); err == nil {
		t.Fatal("rolled_back 终态不可 abandon")
	}
	if _, err := RollbackLLMExperiment(exp.ID); err == nil {
		t.Fatal("不可重复回滚")
	}

	// 形态 B：晋级前用默认模板 → 回滚=停用自定义模板（内容保留、指针回默认）。
	common.DB.Where("module = ?", "recommend").Delete(&model.PromptTemplate{})
	common.DB.Where("1=1").Delete(&model.LLMExperiment{})
	common.DB.Where("1=1").Delete(&model.LLMExperimentRun{})
	common.DB.Where("1=1").Delete(&model.LLMReleaseAudit{})
	exp2 := releaseGateFixture(t)
	if _, err := RunLLMExperimentAudit(context.Background(), exp2.ID); err != nil {
		t.Fatalf("审计: %v", err)
	}
	got2, err := PromoteLLMExperiment(exp2.ID)
	if err != nil || got2.PrePromoteEnabled {
		t.Fatalf("晋级应固化回滚锚（默认态）: %v %+v", err, got2)
	}
	if _, err := RollbackLLMExperiment(exp2.ID); err != nil {
		t.Fatalf("回滚: %v", err)
	}
	if row := userPromptTemplateRow(1, "recommend"); row != nil {
		t.Fatalf("回滚后应回默认模板（无启用中的自定义行）: %+v", row)
	}
	var kept model.PromptTemplate
	if err := common.DB.Where("user_id = 1 AND module = ?", "recommend").First(&kept).Error; err != nil || kept.Enabled {
		t.Fatalf("自定义模板应保留但停用（不删工件）: %v %+v", err, kept)
	}
}

// TestReleaseAuditBudgetAndRegistry 预算/角色登记：release_audit 1500/1，registry 有卡。
func TestReleaseAuditBudgetAndRegistry(t *testing.T) {
	if moduleTokenCap("release_audit", 0) != 1500 || moduleRepairAttempts("release_audit") != 1 {
		t.Fatalf("release_audit 预算应 1500/1: cap=%d repair=%d",
			moduleTokenCap("release_audit", 0), moduleRepairAttempts("release_audit"))
	}
	if _, ok := llmRoleAssets["release_audit"]; !ok {
		t.Fatal("registry 应登记 release_audit 角色")
	}
}
