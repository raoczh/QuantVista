package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func resetStrategyRunJobTests(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	for _, table := range []string{"research_artifacts", "strategy_run_results", "job_failure_notifications", "job_events", "job_steps", "llm_tasks", "job_runs", "screener_strategy_revisions", "screener_strategies"} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理 %s: %v", table, err)
		}
	}
	_ = common.DB.Where("username LIKE ?", "strategy-run-%").Delete(&model.User{}).Error
	t.Cleanup(func() {
		for _, table := range []string{"research_artifacts", "strategy_run_results", "job_failure_notifications", "job_events", "job_steps", "llm_tasks", "job_runs", "screener_strategy_revisions", "screener_strategies"} {
			_ = common.DB.Exec("DELETE FROM " + table).Error
		}
		_ = common.DB.Where("username LIKE ?", "strategy-run-%").Delete(&model.User{}).Error
	})
}

func createStrategyRunUser(t *testing.T, id int64) {
	t.Helper()
	user := &model.User{ID: id, Username: "strategy-run-" + strings.TrimSpace(time.Now().Format("150405.000000000")),
		Role: model.RoleUser, Status: model.StatusEnabled}
	if err := common.DB.Create(user).Error; err != nil {
		t.Fatal(err)
	}
}

func testScanSeed(t *testing.T, name, strategyHash string) (strategyRunSeed, strategyJobSnapshot) {
	t.Helper()
	seed := strategyRunSeed{Kind: JobKindScreenerScan, StrategyIdentity: model.StrategyIdentityBuiltin,
		StrategyKey: "momentum", StrategyRevision: 1, StrategyHash: strategyHash, StrategyName: name}
	seed, snapshot, err := finalizeStrategyRunSeed(seed, ScanRequest{StrategyKey: "momentum", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return seed, snapshot
}

func startTestStrategyRun(t *testing.T, runtime *jobRuntime, userID int64, seed strategyRunSeed,
	snapshot strategyJobSnapshot, handler DurableJobHandler) *model.JobRun {
	t.Helper()
	runtime.registerWithBinding(seed.Kind, time.Minute, handler, strategyRunBinding(), false)
	binding := strategyRunSeedBinding(seed)
	run, err := runtime.startWithBinding(userID, seed.Kind, snapshot, false, nil, nil, &binding)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func successfulScanJobHandler(t *testing.T) DurableJobHandler {
	t.Helper()
	return func(ctx context.Context, userID int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
		row, err := loadStrategyRunForExecution(ctx, userID, JobKindScreenerScan, raw)
		if err != nil {
			return DurableJobResult{}, err
		}
		result := &ScanResult{Strategy: row.StrategyName, StrategyRevision: row.StrategyRevision,
			StrategyHash: row.StrategyHash, TradeDate: "2026-08-06", Universe: 2, Scanned: 2,
			Matched: 1, Items: []ScanHit{{Symbol: "600000", Name: "测试", Price: 10}}}
		return DurableJobResult{Value: result, Status: model.JobStatusSuccess, Total: 2, Succeeded: 1}, nil
	}
}

func TestStrategyRunSuccessArtifactBodyIsolationAndPermission(t *testing.T) {
	resetStrategyRunJobTests(t)
	createStrategyRunUser(t, 9701)
	runtime := newJobRuntime(1, 2)
	defer runtime.close()
	seed, snapshot := testScanSeed(t, "动量", strings.Repeat("a", 64))
	run := startTestStrategyRun(t, runtime, 9701, seed, snapshot, successfulScanJobHandler(t))
	view := waitJobRun(t, 9701, run.ID)
	if view.Status != model.JobStatusSuccess || view.ResultType != JobResultStrategyRun || view.ResultID == nil {
		t.Fatalf("策略扫描 JobRun 未成功收敛: %+v", view)
	}

	var storedRun model.JobRun
	if err := common.DB.First(&storedRun, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"items", "result_json", "tree", "600000"} {
		if strings.Contains(storedRun.RequestSnapshot, forbidden) {
			t.Fatalf("JobRun 快照复制了业务正文 %q: %s", forbidden, storedRun.RequestSnapshot)
		}
	}
	detail, err := GetStrategyRun(9701, JobKindScreenerScan, *view.ResultID)
	if err != nil || detail.Result == nil || !strings.Contains(string(detail.Result), `"symbol":"600000"`) {
		t.Fatalf("本人详情未返回持久正文: detail=%+v err=%v", detail, err)
	}
	if _, err := GetStrategyRun(9702, JobKindScreenerScan, *view.ResultID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("跨用户深链必须隐藏为 not found: %v", err)
	}
	if _, err := GetStrategyRun(9701, JobKindStrategyBacktest, *view.ResultID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("跨结果类型深链必须隐藏为 not found: %v", err)
	}
	rows, err := ListStrategyRuns(9701, JobKindScreenerScan, 20)
	if err != nil || len(rows) != 1 || rows[0].Request != nil || rows[0].Result != nil {
		t.Fatalf("列表必须有界且不加载正文: rows=%+v err=%v", rows, err)
	}

	var artifact model.ResearchArtifact
	if err := common.DB.Where("job_run_id = ? AND type = ?", run.ID, ArtifactTypeScreenerScan).First(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	var resultRow model.StrategyRunResult
	if err := common.DB.First(&resultRow, *view.ResultID).Error; err != nil {
		t.Fatal(err)
	}
	if artifact.StorageRef != "strategy_run_results:"+fmt.Sprint(resultRow.ID) ||
		artifact.ContentHash != resultRow.ContentHash || strings.Contains(artifact.SourceRefs, "600000") {
		t.Fatalf("工件只能保存真实引用与 hash: artifact=%+v row=%+v", artifact, resultRow)
	}
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		if err := persistStrategyRunArtifact(tx, &storedRun, resultRow, time.Now()); err != nil {
			return err
		}
		return persistStrategyRunArtifact(tx, &storedRun, resultRow, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	var artifactCount int64
	common.DB.Model(&model.ResearchArtifact{}).Where("job_run_id = ?", run.ID).Count(&artifactCount)
	if artifactCount != 1 {
		t.Fatalf("重复终态观察不得重复建工件: %d", artifactCount)
	}
	if err := common.DB.Model(&model.StrategyRunResult{}).Where("id = ?", resultRow.ID).
		UpdateColumn("request_json", `{}`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GetStrategyRun(9701, JobKindScreenerScan, resultRow.ID); err == nil ||
		!strings.Contains(err.Error(), "完整性校验失败") {
		t.Fatalf("请求正文被改写后必须拒绝详情读取: %v", err)
	}
}

func TestStrategyRunFailureCancelAndRetryArtifacts(t *testing.T) {
	resetStrategyRunJobTests(t)
	createStrategyRunUser(t, 9711)

	t.Run("failure_then_retry", func(t *testing.T) {
		runtime := newJobRuntime(1, 2)
		defer runtime.close()
		seed, snapshot := testScanSeed(t, "失败后重跑", strings.Repeat("b", 64))
		var calls atomic.Int32
		handler := func(ctx context.Context, userID int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			row, err := loadStrategyRunForExecution(ctx, userID, JobKindScreenerScan, raw)
			if err != nil {
				return DurableJobResult{}, err
			}
			if calls.Add(1) == 1 {
				return DurableJobResult{}, errors.New("fixture failure")
			}
			return DurableJobResult{Value: &ScanResult{Strategy: row.StrategyName, StrategyHash: row.StrategyHash,
				TradeDate: "2026-08-06", Universe: 1, Scanned: 1}, Status: model.JobStatusSuccess}, nil
		}
		parent := startTestStrategyRun(t, runtime, 9711, seed, snapshot, handler)
		if got := waitJobRun(t, 9711, parent.ID); got.Status != model.JobStatusFailed {
			t.Fatalf("父作业应失败: %+v", got)
		}
		var count int64
		common.DB.Model(&model.ResearchArtifact{}).Where("job_run_id = ?", parent.ID).Count(&count)
		if count != 0 {
			t.Fatalf("失败作业不得建工件: %d", count)
		}
		var persistedParent model.JobRun
		if err := common.DB.First(&persistedParent, parent.ID).Error; err != nil {
			t.Fatal(err)
		}
		decoded, err := decodePersistedJobSnapshot(persistedParent)
		if err != nil {
			t.Fatal(err)
		}
		child, err := runtime.startWithBinding(9711, JobKindScreenerScan, json.RawMessage(decoded.Request), false, &parent.ID, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := waitJobRun(t, 9711, child.ID); got.Status != model.JobStatusSuccess {
			t.Fatalf("重跑子作业应成功: %+v", got)
		}
		common.DB.Model(&model.ResearchArtifact{}).Count(&count)
		if count != 1 {
			t.Fatalf("重跑链只应为成功子作业建一个工件: %d", count)
		}
		var parentResult, childResult model.StrategyRunResult
		common.DB.Where("job_run_id = ?", parent.ID).First(&parentResult)
		common.DB.Where("job_run_id = ?", child.ID).First(&childResult)
		if parentResult.RequestJSON != childResult.RequestJSON || parentResult.StrategyHash != childResult.StrategyHash {
			t.Fatalf("重跑必须复制父结果的旧请求与 hash: parent=%+v child=%+v", parentResult, childResult)
		}
	})

	t.Run("cancel_race", func(t *testing.T) {
		for _, table := range []string{"research_artifacts", "strategy_run_results", "job_failure_notifications", "job_events", "job_steps", "job_runs"} {
			_ = common.DB.Exec("DELETE FROM " + table).Error
		}
		runtime := newJobRuntime(1, 2)
		defer runtime.close()
		seed, snapshot := testScanSeed(t, "取消竞态", strings.Repeat("c", 64))
		entered, release := make(chan struct{}), make(chan struct{})
		handler := func(ctx context.Context, userID int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			row, err := loadStrategyRunForExecution(ctx, userID, JobKindScreenerScan, raw)
			if err != nil {
				return DurableJobResult{}, err
			}
			close(entered)
			<-release
			return DurableJobResult{Value: &ScanResult{Strategy: row.StrategyName, StrategyHash: row.StrategyHash,
				TradeDate: "2026-08-06"}, Status: model.JobStatusSuccess}, nil
		}
		run := startTestStrategyRun(t, runtime, 9711, seed, snapshot, handler)
		<-entered
		if _, err := CancelJobRun(9711, run.ID); err != nil {
			t.Fatal(err)
		}
		close(release)
		if got := waitJobRun(t, 9711, run.ID); got.Status != model.JobStatusCanceled {
			t.Fatalf("取消应赢得终态 CAS: %+v", got)
		}
		var row model.StrategyRunResult
		common.DB.Where("job_run_id = ?", run.ID).First(&row)
		if row.Status != model.JobStatusCanceled || row.ResultJSON != "" {
			t.Fatalf("取消结果不得保存临时正文: %+v", row)
		}
		var count int64
		common.DB.Model(&model.ResearchArtifact{}).Count(&count)
		if count != 0 {
			t.Fatalf("取消作业不得建工件: %d", count)
		}
	})
}

func TestPreparedStrategyRunKeepsOldRevisionAfterEdit(t *testing.T) {
	resetStrategyRunJobTests(t)
	userID := int64(9721)
	createStrategyRunUser(t, userID)
	screener := NewScreenerService()
	valueA := 1.0
	strategyA, err := screener.SaveStrategy(userID, SaveStrategyRequest{Name: "版本 A", Period: "swing", Risk: "mid",
		Tree: &CondNode{Factor: "chg_pct", Op: ">", Value: &valueA}})
	if err != nil {
		t.Fatal(err)
	}
	seedA, _, err := screener.prepareScanJob(userID, ScanRequest{StrategyID: strategyA.ID})
	if err != nil {
		t.Fatal(err)
	}
	oldBody := `{"strategy":"版本 A","strategy_revision":1,"strategy_hash":"` + seedA.StrategyHash + `","trade_date":"2026-08-06"}`
	oldResult := &model.StrategyRunResult{UserID: userID, Kind: seedA.Kind,
		StrategyIdentity: seedA.StrategyIdentity, StrategyID: seedA.StrategyID,
		StrategyRevisionID: seedA.StrategyRevisionID, StrategyRevision: seedA.StrategyRevision,
		StrategyHash: seedA.StrategyHash, StrategyName: seedA.StrategyName,
		RequestJSON: seedA.RequestJSON, RequestHash: seedA.RequestHash, ResultJSON: oldBody,
		ContentHash: strings.Repeat("d", 64), Status: model.JobStatusSuccess, JobRunID: 9721001}
	if err := common.DB.Create(oldResult).Error; err != nil {
		t.Fatal(err)
	}
	valueB := 5.0
	strategyB, err := screener.SaveStrategy(userID, SaveStrategyRequest{ID: strategyA.ID, BaseRevisionID: strategyA.CurrentRevisionID,
		Name: "版本 B", Period: "swing", Risk: "mid", Tree: &CondNode{Factor: "chg_pct", Op: ">", Value: &valueB}})
	if err != nil {
		t.Fatal(err)
	}
	if seedA.StrategyRevisionID == nil || *seedA.StrategyRevisionID != strategyA.CurrentRevisionID ||
		seedA.StrategyHash != strategyA.ContentHash || seedA.StrategyHash == strategyB.ContentHash {
		t.Fatalf("已准备结果必须继续引用 A 快照: seed=%+v A=%+v B=%+v", seedA, strategyA, strategyB)
	}
	var normalized ScanRequest
	if err := json.Unmarshal([]byte(seedA.RequestJSON), &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.StrategyRevisionID != strategyA.CurrentRevisionID {
		t.Fatalf("规范化请求被当前 B revision 改写: %+v", normalized)
	}
	executionReq, err := frozenScanRequest(seedA.RequestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if executionReq.StrategyID != 0 || executionReq.StrategyRevisionID != 0 || executionReq.Tree == nil ||
		executionReq.Tree.Value == nil || *executionReq.Tree.Value != valueA {
		t.Fatalf("重放必须直接执行 A 的冻结条件树: %+v", executionReq)
	}
	var stored model.StrategyRunResult
	if err := common.DB.First(&stored, oldResult.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ResultJSON != oldBody || stored.StrategyHash != strategyA.ContentHash {
		t.Fatalf("编辑策略不得改写旧结果正文或 hash: %+v", stored)
	}
}
