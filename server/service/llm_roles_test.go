package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- P1-8 角色资产 registry 锁定测试 ----------

// TestLLMRoleRegistryCoversBudgets registry 与预算表键集双向一致：
// 新增 chatCompletion* 模块必须同时登记两表（llm.go 探针 module=test 有意都不进）。
func TestLLMRoleRegistryCoversBudgets(t *testing.T) {
	for id := range llmModuleBudgets {
		if _, ok := llmRoleAssets[id]; !ok {
			t.Errorf("预算表模块 %q 未登记角色资产（llm_roles.go）", id)
		}
	}
	for id := range llmRoleAssets {
		if _, ok := llmModuleBudgets[id]; !ok {
			t.Errorf("角色资产 %q 未登记预算（llm_budget.go）", id)
		}
	}
	if _, ok := llmRoleAssets["test"]; ok {
		t.Errorf("llm.go 探针 module=test 不应进 registry（探针不是业务角色）")
	}
}

// TestLLMRoleRegistryComplete 每张角色卡必填字段齐全（§4.5.3：缺一项不得进 registry）。
func TestLLMRoleRegistryComplete(t *testing.T) {
	for id, a := range llmRoleAssets {
		if a.RoleID != id {
			t.Errorf("%s: RoleID 与 map key 不一致（%q）", id, a.RoleID)
		}
		if a.Name == "" || a.Version == "" || a.SchemaVersion == "" || a.Purpose == "" {
			t.Errorf("%s: Name/Version/SchemaVersion/Purpose 必填", id)
		}
		if a.Market == "" || a.Trigger == "" || a.Fallback == "" {
			t.Errorf("%s: Market/Trigger/Fallback 必填", id)
		}
		if len(a.InputWhitelist) == 0 || len(a.MustAnswer) == 0 {
			t.Errorf("%s: InputWhitelist/MustAnswer 至少各 1 条", id)
		}
		if len(a.ForbiddenActions) == 0 {
			t.Errorf("%s: ForbiddenActions 至少 1 条", id)
		}
		if len(a.CounterExamples) == 0 {
			t.Errorf("%s: CounterExamples 至少 1 条反例测试坐标", id)
		}
	}
}

// serviceSources 读取本包全部 Go 源码（含/不含测试），供 registry 对拍。
func serviceSources(t *testing.T, tests bool) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		isTest := strings.HasSuffix(name, "_test.go")
		if isTest != tests {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestLLMRoleSchemaAnchors SchemaVersion 与 newLLMRun 调用处真实一致（读源码对拍
// 防手抄漂移）：非测试源码中必须存在 `"<module>", "<schema>"` 相邻形态。
// analysis 特殊（schemaVer 按 panel 分流变量），拆开断言两个 schema 字符串都存在。
func TestLLMRoleSchemaAnchors(t *testing.T) {
	src := serviceSources(t, false)
	for id, a := range llmRoleAssets {
		if id == "analysis" {
			for _, schema := range []string{"analysis.v1", "analysis_panel.v1"} {
				if !strings.Contains(src, `"`+schema+`"`) {
					t.Errorf("analysis: 源码未见 schema %q（newLLMRun 接线变了须同步 registry）", schema)
				}
			}
			continue
		}
		anchor := `"` + id + `", "` + a.SchemaVersion + `"`
		if !strings.Contains(src, anchor) {
			t.Errorf("%s: 源码未见 newLLMRun 锚 %s（schema 变更须同步 registry）", id, anchor)
		}
	}
}

// TestLLMRoleCounterExamplesExist 反例坐标真实存在：每个 CounterExamples 引用的
// 测试函数必须在本包 *_test.go 中定义——挪动/重命名测试须同步 registry
// （llm_golden_test.go 目录同款纪律，这里是机读+程序校验）。
func TestLLMRoleCounterExamplesExist(t *testing.T) {
	src := serviceSources(t, true)
	for id, a := range llmRoleAssets {
		for _, ce := range a.CounterExamples {
			if !strings.HasPrefix(ce, "Test") {
				t.Errorf("%s: 反例坐标 %q 应是测试函数名（Test 前缀）", id, ce)
				continue
			}
			if !strings.Contains(src, "func "+ce+"(") {
				t.Errorf("%s: 反例测试 %q 不存在于本包 *_test.go（挪动测试须同步 registry）", id, ce)
			}
		}
	}
}

// TestLLMRoleNoVoteSynthesis §6.3 反投票合成边界的三重锚：
//   - 全局纪律含「不以角色/名人投票合成最终信号」；
//   - debate_judge 的 ForbiddenActions 含票数合成禁令；
//   - debateJudgeSystem prompt 常量真实携带「不按角色票数平均」纪律（行为侧锚——
//     改 judge prompt 丢掉该句会在此炸出）。
func TestLLMRoleNoVoteSynthesis(t *testing.T) {
	found := false
	for _, d := range llmRoleGlobalDisciplines {
		if strings.Contains(d, "不以角色/名人投票合成最终信号") {
			found = true
		}
	}
	if !found {
		t.Fatalf("全局纪律缺「不以角色/名人投票合成最终信号」（§6.3 不采纳边界）")
	}
	judge := llmRoleAssets["debate_judge"]
	voteBan := false
	for _, f := range judge.ForbiddenActions {
		if strings.Contains(f, "票数") {
			voteBan = true
		}
	}
	if !voteBan {
		t.Fatalf("debate_judge 的 ForbiddenActions 应含票数合成禁令: %+v", judge.ForbiddenActions)
	}
	if !strings.Contains(debateJudgeSystem, "不按角色票数平均") {
		t.Fatalf("debateJudgeSystem prompt 应含「不按角色票数平均」（§6.3 行为锚，删句须评审）")
	}
	// 口头置信度纪律同为 §6.3 边界：全局纪律必须声明「不当真实概率」。
	confBan := false
	for _, d := range llmRoleGlobalDisciplines {
		if strings.Contains(d, "不当真实概率") {
			confBan = true
		}
	}
	if !confBan {
		t.Fatalf("全局纪律缺「模型口头 confidence 不当真实概率」")
	}
}

// TestLLMRoleRegistryOutput Registry() 输出：预算表回填一致、按 RoleID 稳定排序、
// 版本锚跟随代码常量（编译期绑定抽查）。
func TestLLMRoleRegistryOutput(t *testing.T) {
	roles := LLMRoleRegistry()
	if len(roles) != len(llmRoleAssets) {
		t.Fatalf("输出条数应 %d，得到 %d", len(llmRoleAssets), len(roles))
	}
	for i, r := range roles {
		b := moduleBudget(r.RoleID)
		if r.MaxTokens != b.MaxTokens || r.RepairAttempts != b.RepairAttempts {
			t.Errorf("%s: 预算回填与 llmModuleBudgets 不一致（%d/%d vs %d/%d）",
				r.RoleID, r.MaxTokens, r.RepairAttempts, b.MaxTokens, b.RepairAttempts)
		}
		if i > 0 && roles[i-1].RoleID >= r.RoleID {
			t.Errorf("输出应按 RoleID 升序稳定排序: %s >= %s", roles[i-1].RoleID, r.RoleID)
		}
	}
	// 版本锚抽查：常量引用（编译期绑定）——常量递增时 registry 自动跟随。
	byID := map[string]LLMRoleAsset{}
	for _, r := range roles {
		byID[r.RoleID] = r
	}
	if byID["analysis"].Version != analysisPromptVersion {
		t.Errorf("analysis 版本锚应引用 analysisPromptVersion 常量")
	}
	if byID["recommendation"].Version != recPromptVersion {
		t.Errorf("recommendation 版本锚应引用 recPromptVersion 常量")
	}
	if byID["debate_judge"].Version != debateVersion {
		t.Errorf("debate_judge 版本锚应引用 debateVersion 常量")
	}
	if byID["reflection"].Version != reflectionVersion {
		t.Errorf("reflection 版本锚应引用 reflectionVersion 常量")
	}
	if len(LLMRoleDisciplines()) == 0 {
		t.Fatalf("全局纪律不应为空")
	}
}
