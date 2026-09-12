package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestScreenerScanPreservesZeroAndMissingMetrics(t *testing.T) {
	setupTestDB(t)
	cleanWideTables(t)
	resetFactorTable()
	t.Cleanup(resetFactorTable)
	for _, symbol := range []string{"600100", "600200"} {
		seedWideStock(t, symbol, "审查样本", genTrendBars(80, 10, 0.4))
	}
	table, err := ensureFactorTable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, symbol := range table.Symbols {
		value := math.NaN()
		if symbol == "600100" {
			value = 0
		}
		table.cols["turnover_rate"][i], table.cols["pos_60"][i] = value, value
	}
	tree := leafV("close", ">", 0)
	result, err := NewScreenerService().Scan(t.Context(), 713, ScanRequest{Tree: &tree})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("应命中两只样本: %+v", result)
	}
	for _, hit := range result.Items {
		raw, err := json.Marshal(hit)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"turnover_rate", "pos_60"} {
			value, exists := fields[key]
			if hit.Symbol == "600100" && (!exists || value != float64(0)) {
				t.Errorf("合法零值被丢弃: %s %s=%v exists=%v", hit.Symbol, key, value, exists)
			}
			if hit.Symbol == "600200" && exists && value != nil {
				t.Errorf("缺失指标被伪装成数值: %s %s=%v", hit.Symbol, key, value)
			}
		}
	}
}

func TestScreenerSubmissionReplyFailureRollsBack(t *testing.T) {
	resetStrategyRunJobTests(t)
	createStrategyRunUser(t, 713)
	runtime := newJobRuntime(1, 2)
	oldRuntime := defaultJobRuntime
	defaultJobRuntime = runtime
	defer func() { runtime.close(); defaultJobRuntime = oldRuntime }()
	runtime.registerWithBinding(JobKindScreenerScan, time.Minute,
		func(context.Context, int64, bool, json.RawMessage) (DurableJobResult, error) {
			return DurableJobResult{}, errors.New("本地夹具无需执行扫描")
		}, strategyRunBinding(), false)
	const callback = "review_screener_submit_reply"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "strategy_run_results" {
			tx.AddError(errors.New("injected result detail failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	seed, snapshot := testScanSeed(t, "返回读取故障", "test-hash")
	if _, err := startStrategyRunJob(713, seed, snapshot); err == nil {
		t.Fatal("故障注入未生效")
	}
	var count int64
	if err := common.DB.Model(&model.JobRun{}).Where("user_id = ?", 713).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("接口报告提交失败却留下 %d 个后台作业", count)
	}
}

func TestScreenerInvalidStoredTreeCannotPanicViews(t *testing.T) {
	for _, raw := range []string{`{"factor":"rsi_14","op":"between","value":1}`, `{"factor":"rsi_14","op":">"}`, `null`, `{"all":[{"factor":"rsi_14","op":">"}]}`} {
		t.Run(raw, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("历史坏条件使策略视图崩溃: %v", recovered)
				}
			}()
			revision := model.ScreenerStrategyRevision{ID: 11, StrategyID: 1, UserID: 713, TreeJSON: raw}
			current := customStrategyViewFromRevision(1, 11, revision)
			past := strategyRevisionView(revision)
			if current.Tree != nil || past.Tree != nil || len(current.Conditions)+len(past.Conditions) > 0 {
				t.Error("无效条件仍作为可用条件树返回")
			}
		})
	}
}
