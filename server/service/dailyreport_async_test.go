package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// 日报异步任务化（2026-07-14）测试：processing 立即返回与后台回写、复盘/推荐真并行、
// 重生成双败保旧报告、processing 幂等防重。快照与推荐经注入点走假实现（零上游）。

// seedReportEnv 内存库 + 今日开市日历 + 用户 LLM 配置。用户建为 admin——
// httptest 服务器在 127.0.0.1，非 admin 的 allowPrivate=false 会被 SSRF 拦截。
func seedReportEnv(t *testing.T, userID int64, baseURL string) {
	t.Helper()
	setupTestDB(t)
	for _, tbl := range []string{"daily_reports", "llm_configs", "user_quota", "trading_calendars", "alert_rules"} {
		common.DB.Exec("DELETE FROM " + tbl)
	}
	common.DB.Exec("DELETE FROM users WHERE id = ?", userID)
	t.Cleanup(func() {
		for _, tbl := range []string{"daily_reports", "llm_configs", "user_quota", "trading_calendars", "alert_rules"} {
			common.DB.Exec("DELETE FROM " + tbl)
		}
		common.DB.Exec("DELETE FROM users WHERE id = ?", userID)
	})
	common.EncryptionKey = "unit-test-key"
	if err := common.DB.Create(&model.User{ID: userID, Username: "u", Role: model.RoleAdmin, Status: model.StatusEnabled}).Error; err != nil {
		t.Fatalf("造 admin 用户失败: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: today, IsOpen: true}).Error; err != nil {
		t.Fatalf("造日历失败: %v", err)
	}
	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cfg := &model.LLMConfig{UserID: userID, Name: "t", Provider: "openai", BaseURL: baseURL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
}

// fakeReportSvc 组装带注入点的日报服务：快照假实现（零上游）、推荐假实现、
// 固定时钟（今天 16:00，处于 15:35 收盘门槛之后）——不注入时钟的话这批测试的结果
// 取决于执行时刻是上午还是下午（15:35 门槛直接读真实时钟，上午必被拒）。
func fakeReportSvc(recFn func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error)) *DailyReportService {
	return fakeReportSvcAt(recFn, todayAt(16, 0))
}

// todayAt 今天某时某分的本地时刻（测试日历按 time.Now 的今天建，固定时钟须同日）。
func todayAt(hour, min int) time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), hour, min, 0, 0, time.Local)
}

// fakeReportSvcAt 同 fakeReportSvc 但指定固定时钟。
func fakeReportSvcAt(recFn func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error), at time.Time) *DailyReportService {
	svc := NewDailyReportService(nil, nil, nil, nil, nil, NewLLMService(), NewNotifyService())
	svc.snapshotFn = func(ctx context.Context, userID int64, date string) *reportSnapshot {
		return &reportSnapshot{TradeDate: date, Positions: []reportPosition{}, Watch: []reportWatchItem{}, Alerts: []string{}}
	}
	svc.recFn = recFn
	svc.nowFn = func() time.Time { return at }
	return svc
}

// waitReportStatus 轮询等待报告脱离 processing（后台 goroutine 回写），超时 fail。
func waitReportStatus(t *testing.T, id int64) model.DailyReport {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var r model.DailyReport
		if err := common.DB.First(&r, id).Error; err == nil && r.Status != model.ReportStatusProcessing {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("日报 %d 超时未脱离 processing", id)
	return model.DailyReport{}
}

const reportOKReview = `{"choices":[{"message":{"content":"{\"summary\":\"今日震荡\",\"market_review\":\"缩量\",\"events_review\":\"无\",\"position_review\":\"无持仓\",\"watch_review\":\"无自选\",\"risk_warnings\":[\"注意风险\"],\"tomorrow_plan\":\"观望\"}"}}],"usage":{"prompt_tokens":50,"completion_tokens":30,"total_tokens":80}}`

// TestDailyReportAsyncParallel 手动生成：立即返回 processing；复盘与推荐两路证实
// 同时在跑（双方各自挂起等对方进场信号，串行会死锁超时）；推荐失败复盘成功 → partial；
// 手动计 1 次动作。
func TestDailyReportAsyncParallel(t *testing.T) {
	reviewEntered := make(chan struct{})
	recEntered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(reviewEntered)
		select { // 等推荐路也进场，证明两路并行；串行时此处会等满 3s 仍成功（超时保护）
		case <-recEntered:
		case <-time.After(3 * time.Second):
			// 不 fail：交给最后的并行断言判定
		}
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	seedReportEnv(t, 51, srv.URL)

	parallel := false
	svc := fakeReportSvc(func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
		close(recEntered)
		select {
		case <-reviewEntered:
			parallel = true // 复盘路已同时在场
		case <-time.After(3 * time.Second):
		}
		return nil, errors.New("上游超时（模拟）")
	})

	v, err := svc.GenerateFor(context.Background(), 51, true)
	if err != nil {
		t.Fatalf("手动生成应立即返回任务: %v", err)
	}
	if v.Status != model.ReportStatusProcessing || v.ID == 0 {
		t.Fatalf("应返回 processing 报告: %+v", v.DailyReport)
	}

	r := waitReportStatus(t, v.ID)
	if !parallel {
		t.Fatal("复盘与推荐应并行执行（推荐路未观察到复盘路同时在场）")
	}
	if r.Status != model.ReportStatusPartial {
		t.Fatalf("复盘成功+推荐失败应为 partial: %+v", r)
	}
	if !strings.Contains(r.Error, "推荐失败") || strings.Contains(r.Error, "复盘失败") {
		t.Fatalf("错误应只含推荐失败: %q", r.Error)
	}
	if !strings.Contains(r.ReviewJSON, "今日震荡") || r.TotalTokens != 80 {
		t.Fatalf("复盘应落库: %+v", r)
	}
	q := parseQuotaRow(t, 51)
	if q.ActionUsed != 1 {
		t.Fatalf("手动生成应计 1 次动作: %+v", q)
	}
}

// TestDailyReportRegenerateKeepsOld 重生成双败：旧报告内容不被覆盖、状态回滚 success、
// 错误注明「已保留原报告」——替代旧版「先删后生成」的丢失风险。
func TestDailyReportRegenerateKeepsOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 复盘路确定性失败
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	seedReportEnv(t, 52, srv.URL)

	today := time.Now().Format("2006-01-02")
	old := &model.DailyReport{UserID: 52, TradeDate: today, Market: "cn",
		Status: model.ReportStatusSuccess, ReviewJSON: `{"summary":"旧报告内容"}`, TotalTokens: 999}
	if err := common.DB.Create(old).Error; err != nil {
		t.Fatalf("造旧报告失败: %v", err)
	}

	svc := fakeReportSvc(func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("推荐也失败（模拟）")
	})
	v, err := svc.GenerateFor(context.Background(), 52, true)
	if err != nil {
		t.Fatalf("重生成应立即返回任务: %v", err)
	}
	if v.ID != old.ID || v.Status != model.ReportStatusProcessing {
		t.Fatalf("重生成应原地置 processing: %+v", v.DailyReport)
	}

	r := waitReportStatus(t, old.ID)
	if r.Status != model.ReportStatusSuccess {
		t.Fatalf("双败应回滚旧状态 success: %+v", r)
	}
	if !strings.Contains(r.ReviewJSON, "旧报告内容") {
		t.Fatalf("旧报告内容不应被覆盖: %q", r.ReviewJSON)
	}
	if !strings.Contains(r.Error, "已保留原报告") {
		t.Fatalf("错误应注明保留原报告: %q", r.Error)
	}
}

// TestDailyReportProcessingIdempotent 幂等防重：fresh processing 直接复用（零 LLM 调用）；
// 自动路径遇已有报告（含 processing）也直接返回。
func TestDailyReportProcessingIdempotent(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	seedReportEnv(t, 53, srv.URL)

	today := time.Now().Format("2006-01-02")
	processing := &model.DailyReport{UserID: 53, TradeDate: today, Market: "cn", Status: model.ReportStatusProcessing}
	if err := common.DB.Create(processing).Error; err != nil {
		t.Fatalf("造 processing 报告失败: %v", err)
	}

	svc := fakeReportSvc(func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("不应被调用")
	})
	v, err := svc.GenerateFor(context.Background(), 53, true)
	if err != nil {
		t.Fatalf("幂等复用不应报错: %v", err)
	}
	if v.ID != processing.ID || v.Status != model.ReportStatusProcessing {
		t.Fatalf("应复用生成中的报告: %+v", v.DailyReport)
	}
	// 自动路径同样直接返回已有行。
	if av, err := svc.GenerateFor(context.Background(), 53, false); err != nil || av.ID != processing.ID {
		t.Fatalf("自动路径应复用已有行: %+v err=%v", av, err)
	}
	time.Sleep(100 * time.Millisecond) // 若误建后台任务，给它时间打假 LLM
	if calls != 0 {
		t.Fatalf("幂等复用不应触发 LLM 调用: %d", calls)
	}
	var cnt int64
	common.DB.Model(&model.DailyReport{}).Where("user_id = ?", 53).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("不应创建第二行: %d", cnt)
	}
}

// TestDailyReportManualBefore1535Rejected 收盘门槛（固定时钟，不依赖执行时刻）：
// 15:34 手动生成被拒且不落任何报告行；15:35 整放行（同自动窗口起点）。
func TestDailyReportManualBefore1535Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	seedReportEnv(t, 54, srv.URL)

	rec := func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("推荐失败（模拟）")
	}
	// 15:34：拒绝，错误须说明门槛，且不建报告行。
	svc := fakeReportSvcAt(rec, todayAt(15, 34))
	if _, err := svc.GenerateFor(context.Background(), 54, true); err == nil || !strings.Contains(err.Error(), "15:35") {
		t.Fatalf("15:34 手动生成应被拒并说明门槛: err=%v", err)
	}
	var cnt int64
	common.DB.Model(&model.DailyReport{}).Where("user_id = ?", 54).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("被拒时不应落报告行: %d", cnt)
	}
	// 15:35 整：放行（返回 processing 任务）。
	svc = fakeReportSvcAt(rec, todayAt(15, 35))
	v, err := svc.GenerateFor(context.Background(), 54, true)
	if err != nil {
		t.Fatalf("15:35 应放行: %v", err)
	}
	waitReportStatus(t, v.ID)
}

// TestDailyReportNonTradingDayRejected 非交易日（日历 is_open=false）固定时钟拒绝，
// 与执行时刻无关。
func TestDailyReportNonTradingDayRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	seedReportEnv(t, 55, srv.URL)
	today := time.Now().Format("2006-01-02")
	common.DB.Model(&model.TradingCalendar{}).Where("market = ? AND trade_date = ?", "cn", today).
		Update("is_open", false)

	svc := fakeReportSvcAt(func(ctx context.Context, userID int64, allowPrivate bool, req RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("不应被调用")
	}, todayAt(16, 0))
	if _, err := svc.GenerateFor(context.Background(), 55, true); err == nil || !strings.Contains(err.Error(), "休市") {
		t.Fatalf("非交易日应被拒: err=%v", err)
	}
}
