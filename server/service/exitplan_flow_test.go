package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func seedExitPlanningMarket(t *testing.T, now time.Time, symbol string) {
	t.Helper()
	var bars []model.DailyBar
	var calendar []model.TradingCalendar
	for i := 120; i >= 0; i-- {
		day := now.AddDate(0, 0, -i)
		date := day.Format("2006-01-02")
		open := day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
		calendar = append(calendar, model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: open})
		if open && (i > 0 || now.Hour() >= 15) {
			bars = append(bars, model.DailyBar{Market: "cn", Symbol: symbol, TradeDate: date, Open: 10, High: 10.2, Low: 9.8, Close: 10, Volume: 100000, Source: "eastmoney"})
		}
	}
	if err := common.DB.Create(&calendar).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&bars).Error; err != nil {
		t.Fatal(err)
	}
}

func TestExitPlanCreatePreviewAddAndReduceUseRealLedger(t *testing.T) {
	setupTestDB(t)
	now := time.Now().In(time.Local)
	seedExitPlanningMarket(t, now, "600901")
	svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{hook: func() {}})))
	const uid int64 = 881
	batch, rec := seedLinkFixture(t, uid, "600901", model.RecTypeShortTerm, model.RecStatusSuccess)
	if err := common.DB.Model(&model.RecommendationBatch{}).Where("id = ?", batch.ID).Update("score_profile", "momentum").Error; err != nil {
		t.Fatal(err)
	}
	in := PositionInput{Symbol: "600901", Market: "cn", PositionType: model.PositionTypeShortTerm, BuyPrice: 10, Quantity: 1000, BuyFee: 5,
		BuyDate: now.AddDate(0, 0, -2).Format("2006-01-02"), RecommendationID: rec.ID}
	preview, err := svc.PreviewPositionExitPlan(t.Context(), uid, in)
	if err != nil || preview.DataStatus != "ready" || preview.Profile != "momentum" {
		t.Fatalf("实际输入与推荐策略应形成完整预览: %+v %v", preview, err)
	}
	for _, table := range []any{&model.Position{}, &model.PositionTrade{}, &model.PositionExitAssessment{}, &model.PositionExitNotice{}} {
		var count int64
		if err := common.DB.Model(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("预览不能创建账本或提醒")
		}
	}
	if _, err := svc.PreviewPositionExitPlan(t.Context(), uid+1, in); err == nil {
		t.Fatal("不能预览其他用户的来源推荐")
	}
	p, err := svc.Create(t.Context(), uid, in)
	if err != nil {
		t.Fatal(err)
	}
	seed := decodeExitSeed(p.ExitPlanSeedJSON)
	if seed == nil || seed.Profile != "momentum" || seed.Cost != p.RemainingCost {
		t.Fatalf("建仓必须冻结本笔真实费用和策略: %+v", seed)
	}
	original := p.ExitPlanSeedJSON
	reduced, err := svc.AddTradeContext(t.Context(), uid, p.ID, PositionTradeInput{Side: model.PositionTradeSell, Price: 12, Quantity: 500, Fee: 5, TradeDate: now.AddDate(0, 0, -1).Format("2006-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	if reduced.ExitPlanSeedJSON != original {
		t.Fatal("减仓不能重定义初始风险和目标")
	}
	previewInput := in
	previewInput.Quantity = reduced.Quantity
	previewInput.BuyFee = reduced.BuyFee
	previewInput.BuyTax = reduced.BuyTax
	partialPreview, err := svc.PreviewPositionExitPlan(t.Context(), uid, previewInput, p.ID)
	if err != nil || partialPreview.Cost != reduced.RemainingCost {
		t.Fatalf("编辑预览须读取剩余精确成本，不能重复计入原始费用: %+v %v", partialPreview, err)
	}
	added, err := svc.AddTradeContext(t.Context(), uid, p.ID, PositionTradeInput{Side: model.PositionTradeBuy, Price: 11, Quantity: 500, Fee: 5, TradeDate: now.AddDate(0, 0, -1).Format("2006-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	newSeed := decodeExitSeed(added.ExitPlanSeedJSON)
	if newSeed == nil || newSeed.Hash == seed.Hash || newSeed.EntryPrice != added.BuyPrice || newSeed.Cost != added.RemainingCost || newSeed.Source != "add_buy" {
		t.Fatalf("加仓必须按新的实际成本重新规划: %+v", newSeed)
	}
}

func TestExitPlanHoldingEvaluationSeedsOldPositionAndHonorsCashAdjustment(t *testing.T) {
	setupTestDB(t)
	p, _, now := exitPlanFixture()
	seedExitPlanningMarket(t, now, p.Symbol)
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionExitAssessmentService{}
	quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(now, 10, 10.2, 9.8)}
	if _, err := svc.EvaluateUserWithSnapshot(t.Context(), p.UserID, []model.Position{p}, quotes, model.PositionExitSessionIntraday, now); err != nil {
		t.Fatal(err)
	}
	view, err := LatestPositionExitAssessment(t.Context(), p.UserID, p.ID)
	if err != nil || view.ExitPlan == nil || view.Level != model.PositionExitLevelNormal || view.DataStatus != model.PositionExitDataReady {
		t.Fatalf("有效旧仓首次规划不能永久变成数据未知: %+v %v", view, err)
	}
	before := exitPlanCorporateSnapshot{Seed: mustPositionExitJSON(view.ExitPlan.Initial)}
	p.ExitPlanSeedJSON = before.Seed
	adj := model.PositionCorpAdjust{DividendPretax: 1}
	applyExitCorporatePrices(&p, before, &adj)
	chosen := positionExitSeedFor(nil, p, view.ExitPlan, nil, now, nil)
	if chosen.Hash == view.ExitPlan.Initial.Hash || chosen.StopPrice >= view.ExitPlan.Initial.StopPrice {
		t.Fatal("纯现金分红不改变成本时，也必须采用折算后的新价格口径")
	}
}

type retryExitNotifier struct {
	recordingAlertNotifier
	fail bool
}

func (n *retryExitNotifier) SendDurableMsgContext(ctx context.Context, userID int64, msg NotifyMessage) error {
	if n.fail {
		return errors.New("injected durable handoff failure")
	}
	n.SendMsgContext(ctx, userID, msg)
	return nil
}

func TestExitPlanNoticeSameDayMilestonesRetryAndStableTodo(t *testing.T) {
	setupTestDB(t)
	p, s, now := exitPlanFixture()
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.UserPreference{UserID: p.UserID, EnableNotify: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.NotifyChannel{UserID: p.UserID, Kind: model.NotifyKindWebhook, Name: "隔离通知", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	notifier := &retryExitNotifier{recordingAlertNotifier: recordingAlertNotifier{userID: p.UserID}, fail: true}
	svc := &PositionExitAssessmentService{notify: notifier}
	makeRow := func(price float64, previous *ExitPlan, at time.Time) model.PositionExitAssessment {
		in := positionExitInput{position: p, quote: freshExitQuote(at, price, price, price), barRows: exitTestBars(at, 10, 10.2, 9.8, 70),
			now: at, session: model.PositionExitSessionIntraday, planSeed: &s, previousPlan: previous}
		row := evaluatePositionExit(in, defaultPositionExitParams)
		if created, _, err := persistPositionExitAssessment(t.Context(), &row); err != nil || !created {
			t.Fatalf("事件应保存: %v %v", created, err)
		}
		return row
	}
	first := makeRow(s.TargetPrice, nil, now)
	if first.PrimarySignal != "target_first" {
		t.Fatalf("第一目标没有产生提醒: %+v", first)
	}
	if svc.dispatchExitNotices(t.Context(), p.UserID) == nil {
		t.Fatal("交接失败需要保留可重试状态")
	}
	var notice model.PositionExitNotice
	if err := common.DB.First(&notice).Error; err != nil || notice.Status != "pending" || notice.Attempts != 1 {
		t.Fatalf("没有保留失败台账: %+v %v", notice, err)
	}
	if err := common.DB.Model(&notice).Update("lease_until", time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)).Error; err != nil {
		t.Fatal(err)
	}
	notifier.fail = false
	if err := svc.dispatchExitNotices(t.Context(), p.UserID); err != nil {
		t.Fatal(err)
	}
	if err := svc.dispatchExitNotices(t.Context(), p.UserID); err != nil || notifier.calls.Load() != 1 {
		t.Fatal("交接恢复后同一事件只能投递一次")
	}
	second := makeRow(s.ExtendedTarget, decodeExitPlan(first.PlanJSON, first.PlanHash), now.Add(time.Minute))
	if err := svc.dispatchExitNotices(t.Context(), p.UserID); err != nil || notifier.calls.Load() != 2 || second.ActionKey == first.ActionKey {
		t.Fatalf("同日同级的第二目标不能被 Guard 日级去重吞掉: %v", err)
	}
	next := makeRow(s.ExtendedTarget, decodeExitPlan(second.PlanJSON, second.PlanHash), now.Add(24*time.Hour))
	if err := svc.dispatchExitNotices(t.Context(), p.UserID); err != nil || notifier.calls.Load() != 2 {
		t.Fatal("同一持续目标不能逐日重发")
	}
	item := todoItemFromSource(second.TradeDate, TodoItem{Kind: TodoKindPositionExit, RefID: second.ID}, &second)
	itemNext := todoItemFromSource(next.TradeDate, TodoItem{Kind: TodoKindPositionExit, RefID: next.ID}, &next)
	if item.SourceVersion != itemNext.SourceVersion || item.eventDate != itemNext.eventDate {
		t.Fatal("日期变化不能让同一已处理事项重新出现")
	}
}

func TestExitPlanFixedStopRecoversWithoutReusingEarlierDailyLow(t *testing.T) {
	p, s, now := exitPlanFixture()
	o := exitPlanObservation{Position: p, Seed: s, Price: s.StopPrice - 0.1, High: 10, Low: s.StopPrice - 0.1, TradeDate: "2026-09-11", Now: now, PriceOK: true}
	first := evolveExitPlan(o)
	if !first.StopActive {
		t.Fatal("初始止损应触发")
	}
	o.Previous = &first
	o.Price = 10
	o.Now = now.Add(time.Minute)
	recovered := evolveExitPlan(o)
	if recovered.StopActive {
		t.Fatal("已观测过的当日最低不能阻止当前报价恢复")
	}
	o.Previous = &recovered
	o.Now = now.Add(2 * time.Minute)
	quiet := evolveExitPlan(o)
	if quiet.StopActive {
		t.Fatal("先前低点不能伪造第二次触发")
	}
	o.Previous = &quiet
	o.Price = s.StopPrice - 0.1
	o.Now = now.Add(3 * time.Minute)
	again := evolveExitPlan(o)
	if !again.StopActive || again.StopEpisode != 2 {
		t.Fatal("回升后真实再次跌破应形成新事件")
	}
}

func TestExitPlanAIFocusIncludesProfitableProtectionAndDoesNotMutateFact(t *testing.T) {
	p, s, now := exitPlanFixture()
	plan := evolveExitPlan(exitPlanObservation{Position: p, Seed: s, Price: 14, High: 14, Low: 14, ATR: s.ATR14, TradeDate: "2026-09-11", Now: now, PriceOK: true, TechnicalOK: true})
	assessment := &PositionExitAssessmentView{PositionExitAssessment: model.PositionExitAssessment{Level: model.PositionExitLevelUrgent}, ExitPlan: &plan, Evidence: []string{"冻结证据"}}
	input, summary := positionAdvicePlanning(PositionView{ExitAssessment: assessment})
	if summary == nil || summary.CurrentStop != plan.CurrentStop || input.ExitPlan != nil || assessment.ExitPlan == nil {
		t.Fatal("AI 摘要应保留实际保护价而不改写完整评估")
	}
	rows := []positionAdviceRow{{PositionID: 1, PnlPct: -20}, {PositionID: 2, PnlPct: 20, ExitAssessment: input}}
	sortAdviceRowsByUrgency(rows)
	if rows[0].PositionID != 2 {
		t.Fatal("盈利保护触发不能被无程序信号的浮亏挤出 AI 复核预算")
	}
}

func TestExitPlanNoteOnlyEditDoesNotReplaceLegacyObservedPlan(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithLedger(t, 884, "600901", 10, 1000, 5, 0, "2026-09-01")
	if p.ExitPlanSeedJSON != "" {
		t.Fatal("本用例要求没有建仓快照的旧仓")
	}
	in := PositionInput{PositionType: p.PositionType, BuyPrice: p.BuyPrice, BuyDate: p.BuyDate, Quantity: p.Quantity, BuyFee: p.BuyFee, BuyTax: p.BuyTax, UserNote: "只改备注"}
	out, err := NewPositionService(nil).UpdateContext(t.Context(), p.UserID, p.ID, in)
	if err != nil || out.ExitPlanSeedJSON != "" {
		t.Fatalf("无关编辑不能替换已在评估中建立的规划: %v", err)
	}
}

func TestExitPlanUnreadablePriorStateCannotResetProtection(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "transient_read_failure"
		if broken {
			name = "corrupted_plan"
		}
		t.Run(name, func(t *testing.T) {
			setupTestDB(t)
			p, seed, now := exitPlanFixture()
			seedExitPlanningMarket(t, now, p.Symbol)
			p.ExitPlanSeedJSON = mustPositionExitJSON(seed)
			if err := common.DB.Create(&p).Error; err != nil {
				t.Fatal(err)
			}
			row := evaluatePositionExit(positionExitInput{position: p, planSeed: &seed,
				quote: freshExitQuote(now, 14, 14, 14), now: now, session: model.PositionExitSessionIntraday,
				barRows: exitTestBars(now, 10, 10.2, 9.8, 70)}, defaultPositionExitParams)
			if inserted, _, err := persistPositionExitAssessment(t.Context(), &row); err != nil || !inserted {
				t.Fatalf("先建立盈利保护: inserted=%v err=%v", inserted, err)
			}
			before := decodeExitPlan(row.PlanJSON, row.PlanHash)
			if before == nil || before.CurrentStop <= seed.StopPrice {
				t.Fatal("用例必须先有高于初始止损的保护价")
			}
			const hook = "exit_plan_previous_read_failure"
			if broken {
				if err := common.DB.Model(&model.PositionExitAssessment{}).Where("id = ?", row.ID).Update("plan_hash", "damaged").Error; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*model.PositionExitAssessment); ok && len(tx.Statement.Selects) > 0 {
						tx.AddError(errors.New("injected previous plan read failure"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			}
			svc := &PositionExitAssessmentService{}
			at := now.Add(time.Minute)
			quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(at, 13, 13, 13)}
			for i := 0; i < 2; i++ {
				if created, err := svc.EvaluateUserWithSnapshot(t.Context(), p.UserID, []model.Position{p}, quotes, model.PositionExitSessionIntraday, at); err == nil || created != 0 {
					t.Fatalf("前次规划不可核验时不得提交无规划事实: created=%d err=%v", created, err)
				}
			}
			var count int64
			if err := common.DB.Model(&model.PositionExitAssessment{}).Where("position_id = ?", p.ID).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("失败重试不能清空已建立的保护历史: count=%d err=%v", count, err)
			}
			if !broken {
				common.DB.Callback().Query().Remove(hook)
				if _, err := svc.EvaluateUserWithSnapshot(t.Context(), p.UserID, []model.Position{p}, quotes, model.PositionExitSessionIntraday, at.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				var latest model.PositionExitAssessment
				if err := common.DB.Where("position_id = ?", p.ID).Order("evaluated_at DESC, id DESC").First(&latest).Error; err != nil {
					t.Fatal(err)
				}
				recovered := decodeExitPlan(latest.PlanJSON, latest.PlanHash)
				if recovered == nil || recovered.Initial.Hash != before.Initial.Hash || recovered.CurrentStop < before.CurrentStop {
					t.Fatal("查询恢复后必须承接既有保护，不回退到初始止损")
				}
			}
		})
	}
}

func TestExitPlanNoticeBrowserSettingFailuresRemainRetryable(t *testing.T) {
	for _, table := range []string{"browser_notification_preferences", "browser_notification_devices", "user_preferences"} {
		t.Run(table, func(t *testing.T) {
			setupTestDB(t)
			p, seed, now := exitPlanFixture()
			if err := common.DB.Create(&p).Error; err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Create(&model.UserPreference{UserID: p.UserID, EnableNotify: true}).Error; err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Create(&model.BrowserNotificationDevice{UserID: p.UserID, DeviceKeyHash: "offline-exit-device", Name: "隔离浏览器", Enabled: true}).Error; err != nil {
				t.Fatal(err)
			}
			row := evaluatePositionExit(positionExitInput{position: p, planSeed: &seed, quote: freshExitQuote(now, 8, 8, 8),
				now: now, session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
			if inserted, _, err := persistPositionExitAssessment(t.Context(), &row); err != nil || !inserted {
				t.Fatalf("应先生成持仓保护事件: inserted=%v err=%v", inserted, err)
			}
			const hook = "exit_notice_settings_read_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(errors.New("injected notification settings read failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
			notifier := &recordingAlertNotifier{userID: p.UserID}
			svc := &PositionExitAssessmentService{notify: notifier}
			if err := svc.dispatchExitNotices(t.Context(), p.UserID); err == nil {
				t.Fatal("配置读取失败不能伪装成用户主动关闭通知")
			}
			var notice model.PositionExitNotice
			if err := common.DB.First(&notice).Error; err != nil || notice.Status != "pending" || notifier.calls.Load() != 0 {
				t.Fatalf("读取故障应保留未交接事件: notice=%+v err=%v", notice, err)
			}
			common.DB.Callback().Query().Remove(hook)
			if err := common.DB.Model(&notice).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
				t.Fatal(err)
			}
			if err := svc.dispatchExitNotices(t.Context(), p.UserID); err != nil || notifier.calls.Load() != 1 {
				t.Fatalf("读取恢复后应完成交接: calls=%d err=%v", notifier.calls.Load(), err)
			}
		})
	}
}

func TestExitPlanDurableHandoffReportsPreferenceReadFailure(t *testing.T) {
	setupTestDB(t)
	const hook = "exit_handoff_preference_read_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*model.UserPreference); ok {
			tx.AddError(errors.New("injected preference read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(hook) })
	notifier := &NotifyService{browser: &BrowserNotificationService{now: time.Now}}
	err := notifier.SendDurableMsgContext(t.Context(), 885, NotifyMessage{BrowserEvents: []BrowserNotificationInput{{
		SourceType: "position_exit_assessment", SourceID: 1, FactKey: "offline-exit-fact", Category: model.BrowserNotifyCategoryExitRisk,
	}}})
	if err == nil {
		t.Fatal("持久交接必须传播总开关读取失败，不能被误标为已发送")
	}
}
