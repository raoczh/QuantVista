package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func auditWithHighAfterDisplayLimit(t *testing.T) string {
	t.Helper()
	findings := make([]releaseAuditFinding, 0, releaseAuditMaxFindings+1)
	for i := 0; i < releaseAuditMaxFindings; i++ {
		findings = append(findings, releaseAuditFinding{Code: fmt.Sprintf("minor_%d", i), Severity: "low", Message: "次要说明"})
	}
	findings = append(findings, releaseAuditFinding{Code: "fabrication", Severity: "high", Message: "任务段要求编造行情数据"})
	raw, err := json.Marshal(releaseAuditResult{Verdict: "pass", Findings: findings, Summary: "模型自报通过"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReleaseAuditReviewKeepsHighBeyondDisplayLimit(t *testing.T) {
	result, err := parseReleaseAudit(auditWithHighAfterDisplayLimit(t))
	if err != nil {
		t.Fatal(err)
	}
	visibleHigh := false
	for _, finding := range result.Findings {
		visibleHigh = visibleHigh || finding.Code == "fabrication"
	}
	if result.Verdict != model.ReleaseAuditFail || !visibleHigh || len(result.Findings) > releaseAuditMaxFindings {
		t.Fatalf("显示上限不能截掉高风险发现并使发布门放行：%+v", result)
	}
}

func TestReleaseAuditReviewTruncatedHighCannotPromote(t *testing.T) {
	setChallengerFlag(t, false)
	cleanReleaseGateTables(t)
	body := auditChatBody(auditWithHighAfterDisplayLimit(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	oldKey := common.EncryptionKey
	t.Cleanup(func() { common.EncryptionKey = oldKey })
	seedAuditAdmin(t, server.URL)
	experiment := releaseGateFixture(t)
	audit, err := RunLLMExperimentAudit(context.Background(), experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Verdict != model.ReleaseAuditFail {
		t.Errorf("审计工件不得遗漏第九项 high：verdict=%s", audit.Verdict)
	}
	if promoted, err := PromoteLLMExperiment(experiment.ID); err == nil {
		t.Fatalf("含 high 发现的实验不得晋级：%+v", promoted)
	}
}

func TestLLMExperimentReviewListDoesNotCreateChampionState(t *testing.T) {
	setupTestDB(t)
	legacy := model.LLMExperiment{UserID: 1, Module: "recommendation", PromptModule: "recommend", Status: model.ExpStatusDraft}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	const callback = "review_experiment_list_readonly"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		writes++
		tx.AddError(errors.New("readonly connection"))
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	rows, err := ListLLMExperiments()
	if err != nil || writes != 0 || len(rows) != 1 || rows[0].BaselineStale == "" {
		t.Fatalf("读取旧实验应返回基线不可核验说明，不得写入新基线行：rows=%+v writes=%d err=%v", rows, writes, err)
	}
}
