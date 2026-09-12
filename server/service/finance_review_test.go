package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedFinanceReview(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	cleanF10(t)
	for _, row := range []any{
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", ReportName: "已披露年报",
			NoticeDate: time.Now().AddDate(0, 0, -30).Format("2006-01-02"), ROE: 12},
		&model.FinanceStatement{Symbol: "600519", Market: "cn", ReportDate: "2025-12-31", TotalAssets: 100000000},
	} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	oldF10, oldStatements := fetchF10, fetchStatements
	fetchF10 = func(context.Context, string) ([]datasource.DcRow, error) { return nil, datasource.ErrNoData }
	fetchStatements = func(context.Context, string) ([]datasource.EMStatementRow, error) { return nil, datasource.ErrNoData }
	t.Cleanup(func() { fetchF10, fetchStatements = oldF10, oldStatements })
}

func TestFinanceReviewOverviewReadFailureIsNotEmpty(t *testing.T) {
	for _, table := range []string{"finance_indicators", "finance_statements"} {
		t.Run(table, func(t *testing.T) {
			seedFinanceReview(t)
			injected := errors.New("模拟财务列表读取故障")
			const hook = "review_finance_list_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					switch tx.Statement.Dest.(type) {
					case *[]model.FinanceIndicator, *[]model.FinanceStatement:
						tx.AddError(injected)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			if _, err := NewFinanceService().FinanceOverview(t.Context(), "600519"); !errors.Is(err, injected) {
				t.Fatalf("财务查询故障不得返回无数据：%v", err)
			}
		})
	}
}

func TestFinanceReviewConsumersExcludeUnpublishedReports(t *testing.T) {
	seedFinanceReview(t)
	for _, row := range []any{
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31", ReportName: "未来公布",
			NoticeDate: time.Now().AddDate(0, 0, 1).Format("2006-01-02"), ROE: 99},
		&model.FinanceIndicator{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", ReportName: "缺少披露证据", ROE: 100},
		&model.FinanceStatement{Symbol: "600519", Market: "cn", ReportDate: "2026-06-30", TotalAssets: 999000000},
	} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	brief := financeBrief(t.Context(), "600519")
	if brief == nil || brief["report"] != "已披露年报" || len(brief["trend"].([]map[string]any)) != 1 ||
		brief["statement_latest"].(map[string]any)["report_date"] != "2025-12-31" {
		t.Errorf("AI 财务证据不能消费未来或尚无披露依据的报告：%v", brief)
	}
	view, err := NewFinanceService().FinanceOverview(t.Context(), "600519")
	if err != nil {
		t.Fatal(err)
	}
	if len(view["indicators"].([]model.FinanceIndicator)) != 1 || len(view["statements"].([]model.FinanceStatement)) != 1 {
		t.Errorf("详情页与分析须使用一致的已披露口径：%v", view)
	}
}

func TestFinanceReviewBriefRejectsStaleAfterFailedRefresh(t *testing.T) {
	seedFinanceReview(t)
	if err := common.DB.Model(&model.FinanceIndicator{}).Where("symbol = ?", "600519").
		Update("updated_at", time.Now().Add(-8*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if brief := financeBrief(t.Context(), "600519"); brief != nil {
		t.Fatalf("过期财务刷新未成功不得继续充当当前 AI 证据：%v", brief)
	}
}

func TestFinanceReviewConsumersRefreshKnownNewReport(t *testing.T) {
	for _, consumer := range []string{"brief", "overview"} {
		t.Run(consumer, func(t *testing.T) {
			seedFinanceReview(t)
			if err := common.DB.Create(&model.DisclosureSchedule{Symbol: "600519", Market: "cn", ReportDate: "2026-03-31",
				IsPublished: true, ActualDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02")}).Error; err != nil {
				t.Fatal(err)
			}
			calls := 0
			fetchF10 = func(context.Context, string) ([]datasource.DcRow, error) {
				calls++
				return []datasource.DcRow{f10Row(t, "2026-03-31", "新披露季报", 20, 10, 10)}, nil
			}
			if consumer == "brief" {
				brief := financeBrief(t.Context(), "600519")
				if brief == nil || brief["report"] != "新披露季报" {
					t.Errorf("已有新披露报告时不能仍用旧年报：%v", brief)
				}
			} else {
				view, err := NewFinanceService().FinanceOverview(t.Context(), "600519")
				if err != nil {
					t.Fatal(err)
				}
				inds := view["indicators"].([]model.FinanceIndicator)
				if len(inds) == 0 || !strings.Contains(inds[len(inds)-1].ReportName, "新披露") {
					t.Errorf("详情页须更新已知过期报告：%v", inds)
				}
			}
			if calls != 1 {
				t.Errorf("已知新报告应触发一次受控刷新：%d", calls)
			}
		})
	}
}

func TestFinanceReviewAnnouncementsRefreshRecentCache(t *testing.T) {
	setupTestDB(t)
	svc := NewFinanceService()
	old := model.Announcement{Symbol: "600519", Market: "cn", ArtCode: "local-old", Title: "昨天的公告",
		NoticeDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02")}
	if err := common.DB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	previous := fetchAnnouncementFeed
	t.Cleanup(func() { fetchAnnouncementFeed = previous })
	calls := 0
	fetchAnnouncementFeed = func(context.Context, string, int) ([]datasource.EMAnnouncement, error) {
		calls++
		return []datasource.EMAnnouncement{{Symbol: "600519", ArtCode: "local-new", Title: "今天的新公告", NoticeDate: time.Now()}}, nil
	}
	for i := 0; i < 2; i++ {
		rows, err := svc.ListAnnouncements(t.Context(), "600519", 20)
		if err != nil || len(rows) != 2 || rows[0].ArtCode != "local-new" {
			t.Fatalf("昨日公告不能让今日新公告被缓存挡住七天：rows=%v err=%v", rows, err)
		}
	}
	if calls != 1 {
		t.Fatalf("同一小时仍应只刷新一次：%d", calls)
	}
}

func TestFinanceReviewAnnouncementsReadFailurePersistsDuringCooldown(t *testing.T) {
	setupTestDB(t)
	previous := fetchAnnouncementFeed
	t.Cleanup(func() { fetchAnnouncementFeed = previous })
	calls := 0
	injected := errors.New("模拟公告上游失败")
	fetchAnnouncementFeed = func(context.Context, string, int) ([]datasource.EMAnnouncement, error) {
		calls++
		return nil, injected
	}
	svc := NewFinanceService()
	for i := 0; i < 2; i++ {
		if rows, err := svc.ListAnnouncements(t.Context(), "600519", 20); !errors.Is(err, injected) {
			t.Errorf("公告读取失败及冷却期间不能显示为没有公告：rows=%v err=%v", rows, err)
		}
	}
	if calls != 1 {
		t.Fatalf("失败后保留冷却避免反复访问上游：%d", calls)
	}
}

func TestFinanceReviewAnnouncementsConcurrentReadersWaitForRefresh(t *testing.T) {
	setupTestDB(t)
	previous := fetchAnnouncementFeed
	t.Cleanup(func() { fetchAnnouncementFeed = previous })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	fetchAnnouncementFeed = func(ctx context.Context, _ string, _ int) ([]datasource.EMAnnouncement, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []datasource.EMAnnouncement{{Symbol: "600519", ArtCode: "local-shared", Title: "同次刷新结果", NoticeDate: time.Now()}}, nil
	}
	svc := NewFinanceService()
	results := make(chan error, 2)
	read := func() {
		rows, err := svc.ListAnnouncements(t.Context(), "600519", 20)
		if err == nil && (len(rows) != 1 || rows[0].ArtCode != "local-shared") {
			err = errors.New("另一请求尚在刷新时提前返回了无公告")
		}
		results <- err
	}
	go read()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("首个请求没有开始刷新")
	}
	go read()
	select {
	case err := <-results:
		unblock()
		<-results
		t.Fatalf("并发读者应等待刷新完成：%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
