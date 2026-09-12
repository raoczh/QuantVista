package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestAlertReviewComparisonsKeepInputPrecision(t *testing.T) {
	cases := []struct {
		name string
		rule model.AlertRule
		in   alertEval
		want bool
	}{
		{"未突破小数前高", model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 2}, alertEval{Price: 4.032, DayHigh: 4.032, Highs: []float64{4.034, 4.033}}, false},
		{"未跌破小数前低", model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpLTE, Period: 2}, alertEval{Price: 4.038, DayLow: 4.038, Lows: []float64{4.037, 4.039}}, false},
		{"真实突破", model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 2}, alertEval{Price: 4.035, DayHigh: 4.035, Highs: []float64{4.034, 4.033}}, true},
		{"真实跌破", model.AlertRule{Kind: model.AlertKindBreakout, Op: model.AlertOpLTE, Period: 2}, alertEval{Price: 4.036, DayLow: 4.036, Lows: []float64{4.037, 4.039}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, message := evaluateAlert(tc.rule, tc.in)
			if got != tc.want {
				t.Errorf("精确边界判断错误：got=%v want=%v message=%s", got, tc.want, message)
			}
		})
	}
	volumes := make([]int64, volumeAvgWindow)
	for i := range volumes {
		volumes[i] = 200
	}
	rule := model.AlertRule{Kind: model.AlertKindVolumeSurge, Op: model.AlertOpGTE, Threshold: 2}
	got, value, _ := evaluateAlert(rule, alertEval{DayVolume: 399, Volumes: volumes})
	if got || math.Abs(value-1.995) > 1e-9 {
		t.Errorf("399/200 尚未达到两倍：hit=%v value=%v", got, value)
	}
}

func alertReviewQuote(symbol string) (*datasource.Quote, quoteFreshInfo, error) {
	now := time.Now()
	return &datasource.Quote{Symbol: symbol, Market: "cn", Price: 11, Open: 10, High: 11, Low: 10, PrevClose: 10, Volume: 400, Source: "local-review", DataTime: now},
		quoteFreshInfo{Status: freshStatusFresh, ExpectedDate: now.Format("2006-01-02"), MarketState: marketStateClosed}, nil
}

func TestAlertReviewLongWindowsUseEnoughBars(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940001
	now := time.Now()
	bars := make([]datasource.Bar, 270)
	for i := range bars {
		bars[i] = datasource.Bar{TradeDate: now.AddDate(0, 0, i-len(bars)+1).Format("2006-01-02"), Open: 10, High: 10, Low: 9.5, Close: 10, Volume: 200}
	}
	requests := []int{}
	svc := &AlertService{market: &fakeAlertMarket{
		getFreshQuote: func(_ context.Context, _, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
			return alertReviewQuote(symbol)
		},
		getDailyBars: func(_ context.Context, _, _ string, limit int) ([]datasource.Bar, error) {
			requests = append(requests, limit)
			return bars[len(bars)-min(len(bars), limit):], nil
		},
	}}
	rules := []model.AlertRule{
		{UserID: userID, Symbol: "600001", Market: "cn", Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 120, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600001", Market: "cn", Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 250, Status: model.AlertStatusActive},
	}
	if err := common.DB.Create(&rules).Error; err != nil {
		t.Fatal(err)
	}
	if hits, err := svc.evaluateRules(t.Context(), rules); err != nil || hits != len(rules) {
		t.Fatalf("已支持的长周期规则应该正常评估：hits=%d err=%v bar_limits=%v", hits, err, requests)
	}
}

func TestAlertReviewCreateCanceledRequestDoesNotWrite(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940002
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&AlertService{}).Create(ctx, userID, AlertInput{Kind: model.AlertKindCostDrawdown, Threshold: 8})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的创建请求应停止：%v", err)
	}
	var count int64
	if err := common.DB.Model(&model.AlertRule{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("取消后仍创建 %d 条规则", count)
	}
}

func TestAlertReviewCreateCountFailureDoesNotWrite(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940003
	failure := errors.New("本机模拟规则计数故障")
	faulted := false
	const hook = "review_alert_rule_count_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, count := tx.Statement.Dest.(*int64); count && tx.Statement.Table == "alert_rules" && !faulted {
			faulted = true
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	_, err := (&AlertService{}).Create(t.Context(), userID, AlertInput{Kind: model.AlertKindCostDrawdown, Threshold: 8})
	if !faulted || !errors.Is(err, failure) {
		t.Errorf("计数失败不能当成零规则继续创建：faulted=%v err=%v", faulted, err)
	}
	var count int64
	if err := common.DB.Model(&model.AlertRule{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("计数故障后仍写入 %d 条规则", count)
	}
}

func TestAlertReviewUpdateCannotRestoreDeletedRule(t *testing.T) {
	for _, action := range []string{"edit", "status"} {
		t.Run(action, func(t *testing.T) {
			setupTestDB(t)
			rule := model.AlertRule{UserID: 940004, Symbol: "600004", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
			if err := common.DB.Create(&rule).Error; err != nil {
				t.Fatal(err)
			}
			deleted := false
			const hook = "review_alert_deleted_after_read"
			if err := common.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if row, ok := tx.Statement.Dest.(*model.AlertRule); ok && row.ID == rule.ID && !deleted {
					deleted = true
					if err := common.DB.Delete(&model.AlertRule{}, rule.ID).Error; err != nil {
						tx.AddError(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			var err error
			if action == "edit" {
				_, err = (&AlertService{}).Update(t.Context(), rule.UserID, rule.ID, AlertInput{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 12})
			} else {
				_, err = (&AlertService{}).SetStatus(t.Context(), rule.UserID, rule.ID, model.AlertStatusPaused)
			}
			if !deleted || err == nil {
				t.Errorf("读取后删除应导致修改失败：deleted=%v err=%v", deleted, err)
			}
			var count int64
			if err := common.DB.Model(&model.AlertRule{}).Where("id = ?", rule.ID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Error("旧表单把已删除的规则重新插入")
			}
		})
	}
}

func TestAlertReviewMissingEvidenceIsNotSuccessfulCheck(t *testing.T) {
	for _, failure := range []string{"quote", "bars", "short", "stale"} {
		t.Run(failure, func(t *testing.T) {
			setupTestDB(t)
			rule := model.AlertRule{UserID: 940005, Symbol: "600005", Market: "cn", Kind: model.AlertKindMA, Op: model.AlertOpGTE, Period: 20, Status: model.AlertStatusActive}
			if err := common.DB.Create(&rule).Error; err != nil {
				t.Fatal(err)
			}
			svc := &AlertService{market: &fakeAlertMarket{
				getFreshQuote: func(_ context.Context, _, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
					if failure == "quote" {
						return nil, quoteFreshInfo{Status: freshStatusUnknown}, datasource.ErrNoData
					}
					return alertReviewQuote(symbol)
				},
				getDailyBars: func(context.Context, string, string, int) ([]datasource.Bar, error) {
					if failure == "bars" {
						return nil, errors.New("本机日线故障")
					}
					n := 20
					if failure == "short" {
						n = 1
					}
					end := time.Now()
					if failure == "stale" {
						end = end.AddDate(0, -2, 0)
					}
					bars := make([]datasource.Bar, n)
					for i := range bars {
						bars[i] = datasource.Bar{TradeDate: end.AddDate(0, 0, i-n+1).Format("2006-01-02"), Open: 10, Close: 10, High: 10, Low: 9, Volume: 100}
					}
					return bars, nil
				},
			}}
			if hits, err := svc.evaluateRules(t.Context(), []model.AlertRule{rule}); err == nil || hits != 0 {
				t.Errorf("缺失或过时的评估输入不能宣称成功：hits=%d err=%v", hits, err)
			}
			if err := common.DB.First(&rule, rule.ID).Error; err != nil {
				t.Fatal(err)
			}
			if rule.LastCheckDate != "" {
				t.Errorf("未完成判断却写入检查日期：%s", rule.LastCheckDate)
			}
		})
	}
}

func TestMySQLAlertReviewConcurrentCreateRespectsLimit(t *testing.T) {
	db := setupMySQLReviewDB(t, model.AllModels()...)
	const userID int64 = 940006
	if err := db.Create(&model.User{ID: userID, Username: "alert_review_create", Status: model.StatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	progress := newOnboardingProgress(userID, OnboardingCurrentVersion, 1)
	progress.Status = model.OnboardingStatusCompleted
	if err := db.Create(&progress).Error; err != nil {
		t.Fatal(err)
	}
	rules := make([]model.AlertRule, maxAlertsPerUser-1)
	for i := range rules {
		rules[i] = model.AlertRule{UserID: userID, Symbol: "600006", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	}
	if err := db.Create(&rules).Error; err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	svc := &AlertService{market: &fakeAlertMarket{getQuote: func(ctx context.Context, _, symbol string) (*datasource.Quote, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return &datasource.Quote{Symbol: symbol, Name: "规则并发"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := svc.Create(ctx, userID, AlertInput{Symbol: "600006", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 10})
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("两次创建没有同时到达取名阶段")
		}
	}
	close(release)
	success := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "上限") {
			t.Errorf("并发失败应由规则上限决定：%v", err)
		}
	}
	var count int64
	if err := db.Model(&model.AlertRule{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if success != 1 || count != maxAlertsPerUser {
		t.Errorf("同一用户并发创建突破规则上限：success=%d count=%d", success, count)
	}
}

func TestAlertReviewDataGapDoesNotSuppressFinancialRules(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940007
	rules := []model.AlertRule{
		{UserID: userID, Symbol: "600007", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive},
		{UserID: userID, Symbol: "600007", Market: "cn", Kind: model.AlertKindEarnDate, Op: model.AlertOpGTE, Threshold: 3, Status: model.AlertStatusActive},
	}
	if err := common.DB.Create(&rules).Error; err != nil {
		t.Fatal(err)
	}
	schedule := model.DisclosureSchedule{Symbol: "600007", Market: "cn", AppointDate: time.Now().Format("2006-01-02"), ReportDate: "2026-06-30", ReportTypeName: "中报"}
	if err := common.DB.Create(&schedule).Error; err != nil {
		t.Fatal(err)
	}
	hits, err := (&AlertService{market: &fakeAlertMarket{}}).EvaluateUser(t.Context(), userID)
	if hits != 1 || err == nil {
		t.Fatalf("行情缺口需报告，但仍应评估可用财报事实：hits=%d err=%v", hits, err)
	}
}

func TestAlertReviewQuoteTradeDateAndRepeatDedup(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 940008
	quoteTime := time.Now().AddDate(0, 0, -1)
	date := quoteTime.Format("2006-01-02")
	rule := model.AlertRule{UserID: userID, Symbol: "600008", Market: "cn", Kind: model.AlertKindBreakout, Op: model.AlertOpGTE, Period: 2, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	svc := &AlertService{market: &fakeAlertMarket{
		getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
			return &datasource.Quote{Price: 11, High: 11, Low: 10, DataTime: quoteTime}, quoteFreshInfo{Status: freshStatusFresh, ExpectedDate: date, MarketState: marketStateClosed}, nil
		},
		getDailyBars: func(context.Context, string, string, int) ([]datasource.Bar, error) {
			bars := make([]datasource.Bar, 3)
			for i := range bars {
				bars[i] = datasource.Bar{TradeDate: quoteTime.AddDate(0, 0, i-2).Format("2006-01-02"), Open: 10, High: 10, Low: 9, Close: 10, Volume: 100}
			}
			return bars, nil
		},
	}}
	for range 2 {
		if hits, err := svc.evaluateRules(t.Context(), []model.AlertRule{rule}); err != nil || hits != 1 {
			t.Fatalf("完整上一交易日行情应正常评估：hits=%d err=%v", hits, err)
		}
	}
	var events []model.AlertEvent
	if err := common.DB.Where("user_id = ?", userID).Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TradeDate != date {
		t.Fatalf("休市日复看不能把同一行情归到新交易日或重复落事件：%+v", events)
	}
	view := toAlertEventView(events[0])
	if !view.ContextAvailable || view.Context.Bar == nil || view.Context.Bar.TradeDate >= date {
		t.Fatalf("突破快照应排除报价所属交易日本身：%+v", view.Context)
	}
}

func TestAlertReviewAmplitudeDistinguishesMissingLowAndFlat(t *testing.T) {
	setupTestDB(t)
	for i, low := range []float64{0, 10} {
		rule := model.AlertRule{UserID: int64(940009 + i), Symbol: "600009", Market: "cn", Kind: model.AlertKindAmplitude, Op: model.AlertOpLTE, Threshold: 1, Status: model.AlertStatusActive}
		if low == 0 {
			rule.Op = model.AlertOpGTE
		}
		if err := common.DB.Create(&rule).Error; err != nil {
			t.Fatal(err)
		}
		svc := &AlertService{market: &fakeAlertMarket{getFreshQuote: func(_ context.Context, _, symbol string) (*datasource.Quote, quoteFreshInfo, error) {
			q, info, err := alertReviewQuote(symbol)
			q.Price, q.High, q.Low = 10, 10, low
			return q, info, err
		}}}
		hits, err := svc.evaluateRules(t.Context(), []model.AlertRule{rule})
		if low == 0 && (hits != 0 || err == nil) {
			t.Errorf("缺最低价不能推导出 100%% 振幅：hits=%d err=%v", hits, err)
		}
		if low == 10 && (hits != 1 || err != nil) {
			t.Errorf("真实零振幅可命中低振幅规则：hits=%d err=%v", hits, err)
		}
	}
}

func TestAlertReviewRejectsUnsupportedMarketKindChanges(t *testing.T) {
	setupTestDB(t)
	for _, kind := range []string{model.AlertKindCostDrawdown, model.AlertKindEarnDate} {
		rule := model.AlertRule{UserID: 940011, Symbol: "00700", Market: "hk", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 300, Status: model.AlertStatusActive}
		if err := common.DB.Create(&rule).Error; err != nil {
			t.Fatal(err)
		}
		_, err := (&AlertService{}).Update(t.Context(), rule.UserID, rule.ID, AlertInput{Kind: kind, Threshold: 3})
		if err == nil {
			t.Errorf("港股规则不能改成仅有 A 股依据的 %s", kind)
		}
		if err := common.DB.First(&rule, rule.ID).Error; err != nil {
			t.Fatal(err)
		}
		if rule.Kind != model.AlertKindPrice {
			t.Errorf("不支持的类型变更仍写入：%s", rule.Kind)
		}
	}
}

func TestAlertReviewThresholdFitsStoredPrecision(t *testing.T) {
	for _, value := range []float64{math.MaxFloat64, 1e16, -1e16, math.Inf(1), math.NaN()} {
		input := AlertInput{Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: value}
		if err := (&AlertService{}).validate(&input); err == nil {
			t.Errorf("超出存储范围的阈值被接受：%v -> %v", value, input.Threshold)
		}
	}
}

func TestAlertReviewPreOpenCheckDoesNotConsumeNextTradeDay(t *testing.T) {
	setupTestDB(t)
	rule := model.AlertRule{UserID: 940012, Symbol: "600012", Market: "cn", Kind: model.AlertKindPrice, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	preOpen := time.Date(2026, 9, 7, 9, 0, 0, 0, time.Local)
	for _, tradeDate := range []string{"2026-09-04", "2026-09-07", "2026-09-07"} {
		checkedAt := preOpen
		if tradeDate == "2026-09-07" {
			checkedAt = preOpen.Add(time.Hour)
		}
		if _, err := persistAlertEvaluation(t.Context(), rule, 11, true, "到价", tradeDate, checkedAt); err != nil {
			t.Fatal(err)
		}
	}
	var events []model.AlertEvent
	if err := common.DB.Where("rule_id = ?", rule.ID).Order("id ASC").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].TradeDate != "2026-09-04" || events[1].TradeDate != "2026-09-07" {
		t.Fatalf("盘前检查上个交易日后，开盘新交易日仍应有独立事件并按交易日去重：%+v", events)
	}
}
