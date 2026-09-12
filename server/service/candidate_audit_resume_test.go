package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func reviewAuditRequest() candidateAuditJobRequest {
	return candidateAuditJobRequest{Version: 1, Market: "cn", SignalDate: auditSignalDate, OutcomeDate: auditOutcomeDate, ParameterHash: candidateAuditParameterHash()}
}

func TestCandidateAuditBindingRejectsDiscoveryReadFailure(t *testing.T) {
	setupTestDB(t)
	want := errors.New("发现读取故障")
	const callback = "review_audit_binding_discovery_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "candidate_discovery_runs" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	raw, err := json.Marshal(reviewAuditRequest())
	if err != nil {
		t.Fatal(err)
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		_, err := registerCandidateAuditBinding().create(tx, &model.JobRun{ID: 701}, raw)
		return err
	})
	if !errors.Is(err, want) {
		t.Errorf("发现存储故障不能当作尚无记录：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.CandidateAuditRun{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("上游读取失败不能创建审计：count=%d err=%v", count, err)
	}
}

func TestCandidateAuditEmptyCommitRejectsChangedStatus(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	run := model.CandidateAuditRun{Market: "cn", SignalDate: auditSignalDate, OutcomeDate: auditOutcomeDate,
		AuditVersion: CandidateAuditVersion, ParameterHash: candidateAuditParameterHash(),
		Status: model.CandidateAuditStatusFailed, StartedAt: now, DataAsOf: now}
	if err := common.DB.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := finishCandidateAuditWithoutItems(t.Context(), &run, model.CandidateAuditStatusPartial, map[string]int{"signal_discovery_missing": 1}); err == nil {
		t.Fatal("运行已不在 processing 时不能报告 partial 提交成功")
	}
}

func TestCandidateAuditCanceledBeforeStartDoesNotCreateRun(t *testing.T) {
	seedCandidateAuditFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ExecuteCandidateAudit(ctx, nil, 0, reviewAuditRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消必须返回明确原因：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.CandidateAuditRun{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("开始前已取消不能创建审计结果：count=%d err=%v", count, err)
	}
}

func TestCandidateAuditCancelBeforeEmptyCommit(t *testing.T) {
	seedCandidateAuditFixture(t)
	if err := common.DB.Where("trade_date = ?", auditSignalDate).Delete(&model.CandidateDiscoveryRun{}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const callback = "review_audit_cancel_empty_commit"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "candidate_discovery_runs" && errors.Is(tx.Error, gorm.ErrRecordNotFound) {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	_, err := ExecuteCandidateAudit(ctx, nil, 0, reviewAuditRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("空结果降级分支也必须遵守取消：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.CandidateAuditRun{}).Where("status = ?", model.CandidateAuditStatusPartial).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("已取消执行不能提交 partial 终态：count=%d err=%v", count, err)
	}
}

func TestCandidateAuditMissingCalendarCannotInventAdjacentDate(t *testing.T) {
	setupTestDB(t)
	for _, date := range []string{"2026-09-07", "2026-09-09"} {
		if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if prior, err := candidateAuditAdjacentSignalDate("cn", "2026-09-09"); err == nil {
		t.Fatalf("缺少周二日历不能把周一当周三的相邻交易日：prior=%s", prior)
	}
}

func TestCandidateAuditWrongResultReferenceCreatesNothing(t *testing.T) {
	seedCandidateAuditFixture(t)
	if _, err := ExecuteCandidateAudit(t.Context(), nil, 98765, reviewAuditRequest()); err == nil {
		t.Fatal("必须拒绝不存在的绑定引用")
	}
	var count int64
	if err := common.DB.Model(&model.CandidateAuditRun{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("引用错误不能留下未绑定的运行：count=%d err=%v", count, err)
	}
}

func TestGlobalResearchLateFailureKeepsSealedFacts(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	jobID := int64(501)
	for i, status := range []string{model.CandidateAuditStatusSuccess, model.CandidateAuditStatusPartial} {
		jobID = 501 + int64(i)*2
		row := model.CandidateAuditRun{Market: "cn", SignalDate: "2026-09-10", OutcomeDate: "2026-09-11", AuditVersion: CandidateAuditVersion,
			ParameterHash: status, JobRunID: &jobID, Status: status, ItemCount: 1, StartedAt: now, DataAsOf: now, FinishedAt: &now}
		if err := common.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		job := &model.JobRun{ID: jobID + 1, ResultID: &row.ID}
		if err := common.DB.Transaction(func(tx *gorm.DB) error {
			return registerCandidateAuditBinding().finishFailure(tx, job, model.JobStatusFailed, "test", "旧执行器迟到失败", now)
		}); err != nil {
			t.Fatal(err)
		}
		if err := common.DB.First(&row, row.ID).Error; err != nil || row.Status != status {
			t.Errorf("迟到失败不能改写已封存审计：status=%s row=%+v err=%v", status, row, err)
		}
	}
	discovery := model.CandidateDiscoveryRun{Market: "cn", TradeDate: "2026-09-11", DiscoveryVersion: DiscoveryVersion,
		FactorVersion: factorSnapshotVersion, ParameterHash: discoveryParameterHash(), JobRunID: &jobID, Status: DiscoveryRunStatusOK, StartedAt: now, AsOf: now, FinishedAt: &now}
	if err := common.DB.Create(&discovery).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Transaction(func(tx *gorm.DB) error {
		return registerDiscoveryBinding().finishFailure(tx, &model.JobRun{ID: jobID + 1, ResultID: &discovery.ID}, model.JobStatusFailed, "test", "旧执行器失败", now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := common.DB.First(&discovery, discovery.ID).Error; err != nil || discovery.Status != DiscoveryRunStatusOK {
		t.Fatalf("迟到失败不能改写已发布发现：%+v err=%v", discovery, err)
	}
}

func TestGlobalResearchWaitCanBeCanceled(t *testing.T) {
	for _, kind := range []string{"audit", "discovery"} {
		t.Run(kind, func(t *testing.T) {
			setupTestDB(t)
			if kind == "audit" {
				candidateAuditRunMu.Lock()
			} else {
				discoveryRunMu.Lock()
			}
			unlock := func() {
				if kind == "audit" {
					candidateAuditRunMu.Unlock()
				} else {
					discoveryRunMu.Unlock()
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			done := make(chan error, 1)
			go func() {
				if kind == "audit" {
					_, err := ExecuteCandidateAudit(ctx, nil, 0, candidateAuditJobRequest{})
					done <- err
				} else {
					_, err := ExecuteDailyDiscovery(ctx, 0, "cn", "", "")
					done <- err
				}
			}()
			select {
			case err := <-done:
				unlock()
				if !errors.Is(err, context.Canceled) {
					t.Errorf("取消原因丢失：%v", err)
				}
			case <-time.After(200 * time.Millisecond):
				unlock()
				<-done
				t.Error("等待全局执行锁期间必须响应取消")
			}
		})
	}
}

func TestMySQLCandidateAuditObservationsUseOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.StockUniverseDaily{}, &model.FactorSnapshotDaily{}, &model.DailyBar{})
	for _, date := range []string{"2026-09-09", "2026-09-10"} {
		price := 10.0
		if date == "2026-09-10" {
			price = 10.5
		}
		for _, row := range []any{
			&model.StockUniverseDaily{Market: "cn", Symbol: "600061", TradeDate: date, Name: "快照测试", Amount: 1e8},
			&model.DailyBar{Market: "cn", Symbol: "600061", TradeDate: date, Open: 10, High: price + 0.1, Low: 9.9, Close: price, Amount: 1e8, Volume: 1e6, Source: "eastmoney"},
		} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Create(&model.FactorSnapshotDaily{Market: "cn", Symbol: "600061", TradeDate: "2026-09-09", LastBarDate: "2026-09-09", FactorVersion: factorSnapshotVersion, FactorsJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_audit_observation_snapshot"
	wrote := false
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "stock_universe_dailies" && !wrote && tx.Error == nil {
			wrote = true
			if err := db.Model(&model.DailyBar{}).Where("trade_date = ?", "2026-09-10").Updates(map[string]any{"high": 11.1, "close": 11}).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	rows, err := loadAuditObservations(t.Context(), nil, "2026-09-09", "2026-09-10", factorSnapshotVersion, map[string]int{}, map[string]int{})
	obs := rows["600061"]
	if !wrote || err != nil || obs.Status != btObserved || obs.NetPct < 4 || obs.NetPct > 6 {
		t.Fatalf("日线收益不能混入读取后提交的新价格：wrote=%v obs=%+v err=%v", wrote, obs, err)
	}
}

func TestMySQLCandidateAuditReportKeepsRunAndItemsSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.CandidateAuditRun{}, &model.CandidateAuditItem{})
	now := time.Now()
	run := model.CandidateAuditRun{Market: "cn", SignalDate: "2026-09-09", OutcomeDate: "2026-09-10", AuditVersion: CandidateAuditVersion,
		ParameterHash: candidateAuditParameterHash(), Status: model.CandidateAuditStatusSuccess, ItemCount: 1, StartedAt: now, DataAsOf: now, FinishedAt: &now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	item := model.CandidateAuditItem{RunID: run.ID, UserID: 14132, BatchID: 1, Symbol: "600061", Market: "cn", AuditType: model.CandidateAuditTypeObservation,
		SignalDate: run.SignalDate, OutcomeDate: run.OutcomeDate, OutcomeStatus: btObserved, NetReturnPct: 1}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_audit_report_snapshot"
	wrote := false
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "candidate_audit_runs" && !wrote && tx.Error == nil {
			wrote = true
			if err := db.Model(&item).Update("net_return_pct", 9).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	report, err := LoadCandidateAuditUserReport(item.UserID, "", 30, t.Context())
	if !wrote || err != nil || report.Outcome.AvgNetPct != 1 {
		t.Fatalf("报表必须保持读运行时的明细快照：wrote=%v report=%+v err=%v", wrote, report, err)
	}
}
