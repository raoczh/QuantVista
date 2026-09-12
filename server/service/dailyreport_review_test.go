package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestDailyReportReviewLatestReadFailureIsNotEmpty(t *testing.T) {
	setupTestDB(t)
	const hook = "review_daily_latest_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_reports" {
			tx.AddError(errors.New("模拟日报读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	if _, err := (&DailyReportService{}).Latest(19101); err == nil {
		t.Fatal("临时查询失败不能返回无日报")
	}
}

func TestDailyReportReviewAutomaticExistenceReadFailureDoesNotRegenerate(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	const userID int64 = 19102
	seedReportEnv(t, userID, srv.URL)
	report := model.DailyReport{UserID: userID, TradeDate: time.Now().Format("2006-01-02"), Market: "cn",
		Status: model.ReportStatusSuccess, ReviewJSON: `{"summary":"已经成功生成过"}`}
	if err := common.DB.Create(&report).Error; err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	const hook = "review_daily_existence_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_reports" && once.CompareAndSwap(false, true) {
			tx.AddError(errors.New("模拟存在性读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := fakeReportSvc(func(context.Context, int64, bool, RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("本地推荐不执行")
	})
	view, err := svc.GenerateFor(t.Context(), userID, false)
	if err == nil && view != nil {
		waitTerminalJobRuns(t, userID, JobResultDailyReport, view.ID, 1)
	}
	if err == nil || calls.Load() != 0 {
		t.Fatalf("存在性查询故障不能触发自动重生成：err=%v llm_calls=%d", err, calls.Load())
	}
}

func TestDailyReportReviewPartialRerunDoesNotMixOldSections(t *testing.T) {
	for index, failed := range []string{"review", "recommendation"} {
		t.Run(failed, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if failed == "review" {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"message":"本地复盘失败"}}`))
					return
				}
				_, _ = w.Write([]byte(reportOKReview))
			}))
			defer srv.Close()
			userID := int64(19103 + index)
			seedReportEnv(t, userID, srv.URL)
			old := model.DailyReport{UserID: userID, TradeDate: time.Now().Format("2006-01-02"), Market: "cn",
				Status: model.ReportStatusSuccess, ReviewJSON: `{"summary":"上次复盘不能混入本次快照"}`, RecommendationBatchID: 99101}
			if err := common.DB.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			svc := fakeReportSvc(func(context.Context, int64, bool, RecommendRequest) (*RecommendationView, error) {
				if failed == "recommendation" {
					return nil, errors.New("本地推荐失败")
				}
				return &RecommendationView{RecommendationBatch: model.RecommendationBatch{ID: 99102, Status: model.RecStatusSuccess}}, nil
			})
			view, err := svc.GenerateFor(t.Context(), userID, true)
			if err != nil {
				t.Fatal(err)
			}
			waitTerminalJobRuns(t, userID, JobResultDailyReport, view.ID, 1)
			var stored model.DailyReport
			if err := common.DB.First(&stored, old.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Status != model.ReportStatusPartial {
				t.Fatalf("单路失败应为部分成功：%+v", stored)
			}
			if failed == "review" && stored.ReviewJSON != "" {
				t.Fatalf("本次复盘失败，不得把旧复盘与本次新快照混合：%s", stored.ReviewJSON)
			}
			if failed == "recommendation" && stored.RecommendationBatchID != 0 {
				t.Fatalf("本次推荐失败，不得把旧批次冒充本次推荐：%d", stored.RecommendationBatchID)
			}
		})
	}
}

func TestDailyReportReviewSnapshotIsDeliveredAsCompleteJSON(t *testing.T) {
	setupTestDB(t)
	var delivered string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		for _, message := range req.Messages {
			if message.Role == "user" {
				delivered = message.Content
			}
		}
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	snapshot := reportSnapshot{TradeDate: "2026-06-01", Note: strings.Repeat("快照说明", 2500), Deficiencies: []string{"必须保留的末尾数据缺口"}}
	raw, _ := json.Marshal(snapshot)
	_, _, _, err := (&DailyReportService{}).callReview(t.Context(), 0, snapshot.TradeDate, promptRuntime{},
		&model.LLMConfig{BaseURL: srv.URL, Model: "local-review"}, "local", true, string(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	data := strings.TrimPrefix(delivered, "今日收盘数据如下（JSON）：\n")
	if !json.Valid([]byte(data)) || !strings.Contains(data, "必须保留的末尾数据缺口") {
		t.Fatalf("送给复盘员的数据必须是完整 JSON 且保留数据缺口：valid=%v chars=%d", json.Valid([]byte(data)), len([]rune(data)))
	}
}

func TestDailyReportReviewDoesNotReplaceResultOwnedByActiveJob(t *testing.T) {
	for _, status := range []string{model.ReportStatusSuccess, model.ReportStatusProcessing} {
		t.Run(status, func(t *testing.T) {
			const userID int64 = 19106
			seedReportEnv(t, userID, "http://127.0.0.1:1")
			fakeReportSvc(nil)
			t.Cleanup(func() { common.DB.Where("user_id = ?", userID).Delete(&model.JobRun{}) })
			report := model.DailyReport{UserID: userID, TradeDate: time.Now().Format("2006-01-02"), Market: "cn", Status: status}
			if status == model.ReportStatusProcessing {
				report.PreviousStatus = model.ReportStatusSuccess
			}
			if err := common.DB.Create(&report).Error; err != nil {
				t.Fatal(err)
			}
			run := model.JobRun{UserID: userID, Kind: JobKindDailyReport, Status: model.JobStatusRunning,
				ResultType: JobResultDailyReport, ResultID: &report.ID}
			if err := common.DB.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			handler, _ := defaultJobRuntime.handler(JobKindDailyReport)
			raw, _ := json.Marshal(dailyReportJobRequest{Version: 1, TradeDate: report.TradeDate, Manual: true})
			err := common.DB.Transaction(func(tx *gorm.DB) error {
				_, err := handler.binding.create(tx, &model.JobRun{UserID: userID}, raw)
				return err
			})
			if err == nil {
				t.Fatalf("原作业仍活动时不能接管 %s 报告并启动第二个生成器", status)
			}
		})
	}
}

func TestDailyReportReviewPersonalReadFailuresRemainVisible(t *testing.T) {
	analysis, account, date := analysisSnapshotReviewEnv(t, "", 10)
	const hook = "review_daily_personal_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "positions" || tx.Statement.Table == "watchlists" {
			tx.AddError(errors.New("模拟本人组合读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := &DailyReportService{market: analysis.market, position: analysis.position, watchlist: NewWatchlistService(analysis.market)}
	snapshot := svc.buildSnapshot(t.Context(), account.UserID, date)
	notes := strings.Join(snapshot.Deficiencies, "；")
	if !strings.Contains(notes, "持仓读取失败") || !strings.Contains(notes, "自选读取失败") {
		t.Fatalf("本人数据查询故障不能表现为无持仓无自选：%s", notes)
	}
}

func TestDailyReportReviewRelatedRecommendationReadFailureIsVisible(t *testing.T) {
	setupTestDB(t)
	report := model.DailyReport{UserID: 19108, TradeDate: "2026-06-01", Market: "cn", Status: model.ReportStatusSuccess,
		RecommendationBatchID: 99201}
	if err := common.DB.Create(&report).Error; err != nil {
		t.Fatal(err)
	}
	const hook = "review_daily_related_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_batches" {
			tx.AddError(errors.New("模拟关联推荐读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	view, err := (&DailyReportService{rec: &RecommendationService{}}).Get(report.UserID, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), "模拟关联推荐读取故障") {
		t.Fatal("已有推荐暂时不可读时，响应必须说明读取故障，不能只返回 recommendation=null")
	}
}

func TestDailyReportReviewAutoDatabaseFailureRemainsRetryable(t *testing.T) {
	const userID int64 = 19109
	seedReportEnv(t, userID, "http://127.0.0.1:1")
	if err := common.DB.Create(&model.UserPreference{UserID: userID, EnableDailyReport: true}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Where("user_id = ?", userID).Delete(&model.UserPreference{}) })
	var once atomic.Bool
	const hook = "review_daily_auto_database"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		_, readingReport := tx.Statement.Dest.(*model.DailyReport)
		if tx.Statement.Table == "daily_reports" && readingReport && once.CompareAndSwap(false, true) {
			tx.AddError(errors.New("模拟生成前的短暂数据库故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	fakeReportSvc(nil).runAutoOnce(t.Context())
	var count int64
	if err := common.DB.Model(&model.DailyReport{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !once.Load() || count != 0 {
		t.Fatalf("生成前临时数据库故障不能落永久失败日报阻断下一轮自动重试：injected=%v rows=%d", once.Load(), count)
	}
}

func TestDailyReportReviewDegradedRecommendationPropagates(t *testing.T) {
	for _, reviewFails := range []bool{false, true} {
		t.Run(fmt.Sprint(reviewFails), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if reviewFails {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(reportOKReview))
			}))
			defer srv.Close()
			const userID int64 = 19110
			seedReportEnv(t, userID, srv.URL)
			batch := model.RecommendationBatch{UserID: userID, Status: model.RecStatusDegraded, Error: "AI 未完成，量化兜底"}
			if err := common.DB.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			svc := fakeReportSvc(func(context.Context, int64, bool, RecommendRequest) (*RecommendationView, error) {
				return &RecommendationView{RecommendationBatch: batch}, nil
			})
			view, err := svc.GenerateFor(t.Context(), userID, true)
			if err != nil {
				t.Fatal(err)
			}
			runs := waitTerminalJobRuns(t, userID, JobResultDailyReport, view.ID, 1)
			var stored model.DailyReport
			if err := common.DB.First(&stored, view.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Status != model.ReportStatusPartial || runs[0].Status != model.JobStatusDegraded ||
				stored.RecommendationBatchID != batch.ID || !strings.Contains(stored.Error, batch.Error) {
				t.Fatalf("降级推荐应保留并传递到日报和作业状态：report=%s job=%s batch=%d error=%s", stored.Status, runs[0].Status, stored.RecommendationBatchID, stored.Error)
			}
		})
	}
}

func TestDailyReportReviewEventReadFailuresRemainVisible(t *testing.T) {
	analysis, account, date := analysisSnapshotReviewEnv(t, "", 10)
	addAnalysisReviewPosition(t, account, date, "600001", model.PositionStatusHolding)
	const hook = "review_daily_event_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "alert_events" || tx.Statement.Table == "news" || tx.Statement.Table == "disclosure_schedules" {
			tx.AddError(errors.New("模拟事件读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := &DailyReportService{market: analysis.market, position: analysis.position, watchlist: NewWatchlistService(analysis.market)}
	snapshot := svc.buildSnapshot(t.Context(), account.UserID, date)
	notes := strings.Join(snapshot.Deficiencies, "；")
	for _, want := range []string{"提醒读取失败", "重要事件读取失败", "披露名单读取失败"} {
		if !strings.Contains(notes, want) {
			t.Errorf("数据故障不能被当成没有事件：missing=%s notes=%s", want, notes)
		}
	}
}

func TestDailyReportReviewGetPreservesReadFailure(t *testing.T) {
	setupTestDB(t)
	readErr := errors.New("模拟详情数据库故障")
	const hook = "review_daily_get_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_reports" {
			tx.AddError(readErr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	if _, err := (&DailyReportService{}).Get(19111, 1); !errors.Is(err, readErr) {
		t.Fatalf("读取故障不能冒充日报不存在：%v", err)
	}
}

func TestDailyReportReviewAutoLLMReadFailureRemainsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reportOKReview))
	}))
	defer srv.Close()
	const userID int64 = 19112
	seedReportEnv(t, userID, srv.URL)
	if err := common.DB.Create(&model.UserPreference{UserID: userID, EnableDailyReport: true}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Where("user_id = ?", userID).Delete(&model.UserPreference{}) })
	var once atomic.Bool
	const hook = "review_daily_auto_llm_read"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "llm_configs" && once.CompareAndSwap(false, true) {
			tx.AddError(errors.New("模拟模型配置短暂读取故障"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	svc := fakeReportSvc(func(context.Context, int64, bool, RecommendRequest) (*RecommendationView, error) {
		return nil, errors.New("本地推荐未生成")
	})
	svc.runAutoOnce(t.Context())
	var count int64
	if err := common.DB.Model(&model.DailyReport{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !once.Load() || count != 0 {
		t.Fatalf("临时配置读取失败不能阻断当天自动重试：injected=%v reports=%d", once.Load(), count)
	}
	svc.runAutoOnce(t.Context())
	var report model.DailyReport
	if err := common.DB.Where("user_id = ?", userID).First(&report).Error; err != nil {
		t.Fatal(err)
	}
	runs := waitTerminalJobRuns(t, userID, JobResultDailyReport, report.ID, 1)
	if runs[0].Status != model.JobStatusDegraded {
		t.Fatalf("下一轮应实际执行并完成复盘：%s", runs[0].Status)
	}
}

func TestTomorrowDisclosuresReviewSelectionReadFailure(t *testing.T) {
	setupTestDB(t)
	injected := errors.New("模拟关注标的集合查询失败")
	const hook = "review_disclosure_selection_read"
	if err := common.DB.Callback().Row().Before("gorm:row").Register(hook, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "watchlist_items") {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Row().Remove(hook) })
	if _, err := TomorrowDisclosures(19115, "2026-09-10"); !errors.Is(err, injected) {
		t.Fatalf("关注集合读取失败不能返回无明日披露：%v", err)
	}
}
