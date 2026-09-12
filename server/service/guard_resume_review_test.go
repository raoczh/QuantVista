package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestGuardMalformedConfigDoesNotEnableNotifications(t *testing.T) {
	for _, raw := range []string{"null", "{bad json", `{"enabled":null}`, `{"enabled":false,"evening":null}`} {
		if cfg := parseGuardConfig(raw); cfg.Enabled {
			t.Errorf("损坏配置不能默认开启守护：raw=%s cfg=%+v", raw, cfg)
		}
		if _, err := normalizeGuardConfigJSON(raw); err == nil {
			t.Errorf("保存配置必须拒绝非对象或空字段：%s", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"pos_pct":8}`, `{"enabled":false}`} {
		stored, err := normalizeGuardConfigJSON(raw)
		if err != nil || parseGuardConfig(stored) != parseGuardConfig(raw) {
			t.Errorf("保存与读取的缺省字段语义必须一致：raw=%s stored=%s err=%v", raw, stored, err)
		}
	}
}

func TestGuardConfigReadFailureDoesNotEnableNotifications(t *testing.T) {
	setupTestDB(t)
	if !loadGuardConfig(14101).Enabled {
		t.Fatal("从未设置守护的用户仍使用默认配置")
	}
	const callback = "review_guard_config_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_preferences" {
			tx.AddError(errors.New("guard preference unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	if loadGuardConfig(14101).Enabled {
		t.Fatal("读取偏好失败不能被当作从未设置而启用默认守护")
	}
}

// 同时保留当日有效事件，防止用清空全部结果掩盖日期/市场过滤错误。
func seedGuardReviewEvents(t *testing.T, today string) {
	t.Helper()
	for i, spec := range []struct{ market, date string }{{"cn", today}, {"cn", mustAddDays(today, 1)}, {"hk", today}} {
		for _, row := range []any{
			&model.Announcement{Symbol: "600061", Market: spec.market, ArtCode: spec.market + spec.date, Title: "事件公告", NoticeDate: spec.date},
			&model.LhbEntry{Symbol: "600061", Market: spec.market, TradeDate: spec.date, ChangeType: "01", NetBuy: -1e8, SellAmt: 2e8},
			&model.EarningsForecast{Symbol: "600061", Market: spec.market, ReportDate: []string{"2026-03-31", "2026-06-30", "2026-09-30"}[i], NoticeDate: spec.date, PredictType: "首亏"},
		} {
			if err := common.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGuardEveningIgnoresFutureAndForeignEvents(t *testing.T) {
	setupTestDB(t)
	const today = "2026-09-10"
	p := seedHoldingWithPeak(t, 14102, "600061", "事件边界", 10, 100, 10, today)
	seedGuardReviewEvents(t, today)
	svc := &GuardService{notify: &NotifyService{channelSender: reviewNotifySender(func(context.Context, string, string, NotifyMessage) error {
		t.Error("测试不应发出外部通知")
		return nil
	})}}
	n := svc.evaluateGuardUserEvening(p.UserID, today, mustAddDays(today, -2))
	var rows []model.GuardEvent
	if err := common.DB.Where("user_id = ?", p.UserID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(rows) != 3 {
		t.Errorf("应仅保留本市场当日三类事实：n=%d rows=%+v", n, rows)
	}
	for _, row := range rows {
		if row.TradeDate != today || strings.Contains(row.Message, "2 条") {
			t.Errorf("不能引入未来或其他市场的公告/龙虎榜/预告：%+v", row)
		}
	}
}

func TestCanceledCorporateActionCannotTriggerReview(t *testing.T) {
	for _, progress := range []string{"取消分配", "董事会预案", "不分配"} {
		rows := []model.CorporateAction{{Symbol: "600061", Market: "cn", ExDate: "2026-09-11", TransferRatio: 10, Progress: progress}}
		if h := evalPosExDiv("600061", "已撤销", rows, "2026-09-10"); h != nil {
			t.Errorf("非实施方案不能作为即将除权事实：progress=%s hit=%+v", progress, h)
		}
		if h := evalSellReviewExDiv(rows, "2026-09-10"); h != nil {
			t.Errorf("非实施方案不能生成除权复核：progress=%s hit=%+v", progress, h)
		}
	}
}

func TestIpoGuardRouteOpensTodayPage(t *testing.T) {
	hits := evalIpoToday([]model.IpoSubscription{{Kind: model.IpoKindStock, Code: "600061", ApplyCode: "730061", ApplyDate: "2026-09-11"}}, "2026-09-11")
	if len(hits) != 1 || hits[0].Route != "/today" {
		t.Fatalf("打新通知应导航到现有今日待办页面：%+v", hits)
	}
}

func TestGuardCanceledBeforeCommitCannotWrite(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	pinCalendarTo(t, today)
	p := seedHoldingWithPeak(t, 14103, "600061", "取消轮次", 10, 100, 10, today)
	if err := common.DB.Model(p).Update("plan_stop_loss", 11).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const callback = "review_guard_cancel_commit"
	entered := false
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "guard_events" {
			entered = true
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	svc := NewGuardService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})))
	n := svc.evaluateGuardUser(ctx, p.UserID, defaultGuardConfig(), today)
	var count int64
	if err := common.DB.Model(&model.GuardEvent{}).Where("user_id = ?", p.UserID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !entered {
		t.Fatal("夹具必须进入真实守护事件写入边界")
	}
	if n != 0 || count != 0 {
		t.Fatalf("轮次在提交前取消不能产生事实：n=%d count=%d", n, count)
	}
}

func TestIpoGuardRunsBeforeCloseWithoutHoldings(t *testing.T) {
	const userID int64 = 14104
	cleanBrowserNotificationTables(t, userID)
	day := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local)
	today := day.Format("2006-01-02")
	pinCalendarTo(t, today)
	if err := common.DB.Create(&model.UserPreference{UserID: userID, EnableNotify: true, GuardConfigJSON: `{"evening":false,"ipo":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: "600061", ApplyCode: "730061", ApplyDate: today, Name: "申购测试"}).Error; err != nil {
		t.Fatal(err)
	}
	var messages []NotifyMessage
	notifier := &NotifyService{browser: &BrowserNotificationService{sender: &fakeBrowserPushSender{}}, channelSender: reviewNotifySender(func(_ context.Context, _, _ string, msg NotifyMessage) error {
		messages = append(messages, msg)
		return nil
	})}
	if _, err := notifier.Create(userID, NotifyChannelInput{Kind: model.NotifyKindWebhook, Name: "仅测试替身", Target: "https://example.com/review", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	svc := &GuardService{notify: notifier}
	svc.runGuardEveningRoundAt(day.Add(10 * time.Hour))
	svc.runGuardRoundAt(day.Add(5 * time.Hour))
	if len(messages) != 0 {
		t.Fatal("收盘后的补跑不能发送今日可申购")
	}
	svc.runGuardRoundAt(day)
	svc.runGuardRoundAt(day.Add(15 * time.Minute))
	if len(messages) != 1 || messages[0].Route != "/today" {
		t.Fatalf("无持仓且关闭盘后提醒的用户仍应在申购时段收到一次打新提醒：%+v", messages)
	}
}

type guardNameReviewAdapter struct{ reviewQuoteHookAdapter }

func (a guardNameReviewAdapter) GetQuote(ctx context.Context, market, symbol string) (*datasource.Quote, error) {
	q, err := a.reviewQuoteHookAdapter.GetQuote(ctx, market, symbol)
	q.Name, q.ChangePct = "ST当日名称", 4.9
	return q, err
}

func TestGuardUsesCurrentQuoteNameForSTLimit(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	pinCalendarTo(t, today)
	p := seedHoldingWithPeak(t, 14105, "600061", "旧名称", 10, 100, 10, today)
	if err := common.DB.Create(&model.WatchlistItem{UserID: p.UserID, Market: "cn", Symbol: "600062", Name: "旧自选名称", IsPinned: true}).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewGuardService(NewMarketService(datasource.NewManagerWithAdapters(guardNameReviewAdapter{reviewQuoteHookAdapter{quoteTime: now, hook: func() {}}})))
	n := svc.evaluateGuardUser(t.Context(), p.UserID, defaultGuardConfig(), today)
	var rows []model.GuardEvent
	if err := common.DB.Where("user_id = ?", p.UserID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(rows) != 2 {
		t.Fatalf("现名为ST时，4.9%%应触发持仓及自选涨停事件：n=%d rows=%+v", n, rows)
	}
	for _, row := range rows {
		if !strings.Contains(row.Message, "涨停") {
			t.Errorf("当前ST涨停语义不能受旧名称影响：%+v", row)
		}
	}
}

func TestGuardEveningReadFailureDoesNotCreatePartialFacts(t *testing.T) {
	setupTestDB(t)
	const today = "2026-09-10"
	p := seedHoldingWithPeak(t, 14106, "600061", "读取失败", 10, 100, 10, today)
	seedGuardReviewEvents(t, today)
	const callback = "review_guard_evening_read_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "lhb_entries" {
			tx.AddError(errors.New("lhb unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	n := (&GuardService{notify: &NotifyService{}}).evaluateGuardUserEvening(p.UserID, today, mustAddDays(today, -2), t.Context())
	var count int64
	if err := common.DB.Model(&model.GuardEvent{}).Count(&count).Error; err != nil || n != 0 || count != 0 {
		t.Fatalf("读取失败必须整轮停止，不能记录部分结果：n=%d count=%d err=%v", n, count, err)
	}
}

func TestGuardUnconfirmedSharesCannotTriggerOldPlanPrice(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	pinCalendarTo(t, today)
	p, _ := seedAdjustCase(t, 14107, today, 0, 10, 0)
	if err := common.DB.Model(p).Update("plan_stop_loss", 15).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewGuardService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})))
	if n := svc.evaluateGuardUser(t.Context(), p.UserID, defaultGuardConfig(), today); n != 0 {
		t.Fatalf("送转未确认时，除权后10元不能触发除权前15元的止损计划：n=%d", n)
	}
}
