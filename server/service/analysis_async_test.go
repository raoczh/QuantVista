package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

const analysisOKResponse = `{"choices":[{"message":{"content":"{\"rating\":\"neutral\",\"confidence\":50,\"summary\":\"市场中性\",\"highlights\":[],\"risks\":[],\"opportunities\":[],\"suggestions\":[],\"anti_thesis\":[\"反方\"],\"kill_switches\":[\"失效条件\"],\"unknowns\":[\"数据盲区\"],\"disclaimer\":\"仅供参考\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func waitAnalysisStatus(t *testing.T, id int64) model.AnalysisRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var rec model.AnalysisRecord
		if err := common.DB.First(&rec, id).Error; err == nil && rec.Status != model.AnalysisStatusProcessing {
			return rec
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("分析任务 %d 超时未脱离 processing", id)
	return model.AnalysisRecord{}
}

// TestAnalysisAsyncShell 锁定 HTTP 入口所需语义：任务先落库立即返回，LLM 在独立
// 后台 context 中继续；重复提交复用同一 processing，最终回写原记录而非另建一行。
func TestAnalysisAsyncShell(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(analysisOKResponse))
	}))
	defer srv.Close()

	const userID int64 = 76
	seedReportEnv(t, userID, srv.URL)
	common.DB.Exec("DELETE FROM analysis_records")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM analysis_records") })

	market := NewMarketService(datasource.NewManagerWithAdapters(refusalTestAdapter{}))
	svc := NewAnalysisService(market, nil, nil, NewLLMService(), nil)
	started := time.Now()
	v, err := svc.AnalyzeAsync(userID, true, AnalyzeRequest{Module: model.AnalysisModuleMarket, Market: "cn"})
	if err != nil {
		t.Fatalf("AnalyzeAsync 应返回任务: %v", err)
	}
	if v.ID == 0 || v.Status != model.AnalysisStatusProcessing {
		t.Fatalf("应立即返回 processing 记录: %+v", v.AnalysisRecord)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("同步段不应等待数据采集/LLM: %v", elapsed)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("后台任务未进入 LLM")
	}
	dup, err := svc.AnalyzeAsync(userID, true, AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "600000"})
	if err != nil || dup.ID != v.ID {
		t.Fatalf("processing 应防重复用: id=%d err=%v", dup.ID, err)
	}
	if err := svc.Delete(userID, v.ID); err == nil || !strings.Contains(err.Error(), "后台执行") {
		t.Fatalf("生成中应拒绝删除: %v", err)
	}

	close(release)
	rec := waitAnalysisStatus(t, v.ID)
	if rec.Status != model.AnalysisStatusSuccess || rec.Summary != "市场中性" || rec.TraceID == "" {
		t.Fatalf("后台终态回写不符: %+v", rec)
	}
	var count int64
	common.DB.Model(&model.AnalysisRecord{}).Where("user_id = ?", userID).Count(&count)
	if count != 1 {
		t.Fatalf("后台应更新原 processing 行，实际记录数 %d", count)
	}
}

func TestAnalysisProcessingStale(t *testing.T) {
	const userID int64 = 77
	seedReportEnv(t, userID, "http://127.0.0.1:1")
	common.DB.Exec("DELETE FROM analysis_records")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM analysis_records") })

	rec := &model.AnalysisRecord{
		UserID: userID, Module: model.AnalysisModuleMarket, Target: "全市场",
		Status: model.AnalysisStatusProcessing, Title: "生成中",
	}
	if err := common.DB.Create(rec).Error; err != nil {
		t.Fatal(err)
	}
	common.DB.Model(rec).Update("updated_at", time.Now().Add(-analysisProcessingStale-time.Minute))

	svc := &AnalysisService{}
	if v := svc.reuseProcessingAnalysis(userID); v != nil {
		t.Fatalf("过期 processing 不应复用: %+v", v)
	}
	var got model.AnalysisRecord
	common.DB.First(&got, rec.ID)
	if got.Status != model.AnalysisStatusFailed || got.ErrorCode != AsyncLLMTaskErrorStale ||
		!strings.Contains(got.Error, "任务中断") {
		t.Fatalf("死任务应惰性转 failed: %+v", got)
	}
}
