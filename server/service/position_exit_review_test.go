package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func seedExitReviewBars(t *testing.T, rows []model.DailyBar) {
	t.Helper()
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPositionExitRepeatedATRStateAndRecross(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	p := seedHoldingWithPeak(t, 12021, "600901", "ATR 连续状态", 8, 100, 11, "2026-07-01")
	bars := exitTestBars(now, 8, 8.2, 7.8, 70)
	last := &bars[len(bars)-1]
	last.Open, last.High, last.Low, last.Close = 10, 10.2, 9.8, 10
	seedExitReviewBars(t, bars)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-08-10", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionExitAssessmentService{}
	evaluate := func(price float64) {
		t.Helper()
		now = now.Add(time.Minute)
		quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(now, price, price, price)}
		if _, err := svc.EvaluateUserWithSnapshot(context.Background(), p.UserID, []model.Position{*p}, quotes, model.PositionExitSessionIntraday, now); err != nil {
			t.Fatal(err)
		}
	}
	countCrossings := func() int64 {
		t.Helper()
		var count int64
		if err := common.DB.Model(&model.PositionExitAssessment{}).Where("position_id = ? AND primary_signal = ?", p.ID, "atr14_break").Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	for i := 0; i < 5; i++ {
		evaluate(9)
	}
	if count := countCrossings(); count != 1 {
		t.Errorf("持续位于 ATR 线下只能产生一次首次跌破，得到 %d 次", count)
	}
	evaluate(10)
	evaluate(9)
	if count := countCrossings(); count != 2 {
		t.Errorf("回升到保护线上方后再次跌破必须记录第二次穿越，得到 %d 次", count)
	}
}

func TestPositionExitRejectsStaleTechnicalBars(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-08-10", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	seedExitReviewBars(t, exitTestBars(now.AddDate(0, 0, -14), 10, 10.2, 9.8, 70))
	for i, session := range []string{model.PositionExitSessionIntraday, model.PositionExitSessionClose} {
		t.Run(session, func(t *testing.T) {
			p := seedHoldingWithPeak(t, int64(12022+i), "600901", "过时日线", 10, 100, 12, "2026-07-01")
			svc := &PositionExitAssessmentService{}
			quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(now, 8, 8, 8)}
			if _, err := svc.EvaluateUserWithSnapshot(context.Background(), p.UserID, []model.Position{*p}, quotes, session, now); err != nil {
				t.Fatal(err)
			}
			var row model.PositionExitAssessment
			if err := common.DB.Where("position_id = ?", p.ID).Order("id DESC").First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Level != model.PositionExitLevelUnknown || row.DataStatus != model.PositionExitDataPartial || row.ShouldTodo || row.MA60 != 0 {
				t.Fatalf("两周前的日线不能产生当前确定的技术风险：level=%s data=%s todo=%v MA60=%v gaps=%s", row.Level, row.DataStatus, row.ShouldTodo, row.MA60, row.DataGapsJSON)
			}
			if err := common.DB.Model(p).Update("plan_stop_loss", 9).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := svc.EvaluateUserWithSnapshot(context.Background(), p.UserID, []model.Position{*p}, quotes, session, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			row = model.PositionExitAssessment{}
			if err := common.DB.Where("position_id = ?", p.ID).Order("id DESC").First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Level != model.PositionExitLevelUrgent || row.PrimarySignal != "plan_stop" || row.DataStatus != model.PositionExitDataPartial {
				t.Fatalf("过时日线不应吞掉 fresh 行情独立确认的计划止损：%+v", row)
			}
		})
	}
}

func TestPositionExitLatestDoesNotReuseChangedPosition(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	p := seedHoldingWithPeak(t, 12024, "600901", "旧持仓评估", 10, 100, 12, "2026-07-01")
	in := positionExitInput{position: *p, quote: freshExitQuote(now, 8, 8, 8), now: now, session: model.PositionExitSessionIntraday,
		rules: []model.AlertRule{{Kind: model.AlertKindCostDrawdown, Threshold: 10}}}
	row := evaluatePositionExit(in, defaultPositionExitParams)
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &row); err != nil || !inserted {
		t.Fatalf("保存原始风险：inserted=%v err=%v", inserted, err)
	}
	if err := common.DB.Model(p).Updates(map[string]any{"buy_price": 5, "remaining_cost": 500}).Error; err != nil {
		t.Fatal(err)
	}
	latest, err := LatestPositionExitAssessments(context.Background(), p.UserID, []int64{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if previous, ok := latest[p.ID]; ok {
		t.Fatalf("成本已从10改为5，不能把旧亏损评估继续作为当前持仓事实：%+v", previous)
	}
	if historical, err := PositionExitAssessmentByID(context.Background(), p.UserID, p.ID, row.ID); err != nil || historical.BuyPrice != 10 {
		t.Fatalf("原始历史评估仍应可精确读取：historical=%+v err=%v", historical, err)
	}
}

func TestPositionExitChangedBasisCreatesNewFact(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	p := seedHoldingWithPeak(t, 12025, "600901", "相同等级新依据", 10, 100, 12, "2026-07-01")
	in := positionExitInput{position: *p, quote: freshExitQuote(now, 10, 10, 10), now: now, session: model.PositionExitSessionIntraday}
	first := evaluatePositionExit(in, defaultPositionExitParams)
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &first); err != nil || !inserted {
		t.Fatalf("首次保存：%v %v", inserted, err)
	}
	if err := common.DB.Model(p).Updates(map[string]any{"buy_price": 9, "remaining_cost": 900}).Error; err != nil {
		t.Fatal(err)
	}
	in.position, in.now = *p, now.Add(time.Minute)
	second := evaluatePositionExit(in, defaultPositionExitParams)
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &second); err != nil || !inserted {
		t.Fatalf("持仓输入已变化，即使等级相同也要保存可对应新持仓的评估：inserted=%v err=%v", inserted, err)
	}
}

func TestPositionExitEvidencePreservesPricePrecision(t *testing.T) {
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	row := evaluatePositionExit(positionExitInput{
		position: model.Position{UserID: 12026, Market: "cn", BuyPrice: 4.1, PeakPrice: 4.1, PlanStopLoss: 4.0375},
		quote:    freshExitQuote(now, 4.035, 4.036, 4.034), now: now, session: model.PositionExitSessionIntraday,
	}, defaultPositionExitParams)
	if !strings.Contains(row.PrimaryReason, "4.034") || !strings.Contains(row.PrimaryReason, "4.0375") {
		t.Fatalf("风险证据必须保留实际价格精度：%s", row.PrimaryReason)
	}
}

func TestPositionExitATRComparisonUsesUnroundedLine(t *testing.T) {
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	bars := exitTestBars(now, 4.039, 4.0397, 4.0384, 70)
	bars[len(bars)-1].High = 4.0403
	// ATR=(13*0.0013+0.0019)/14，真实保护线约 4.0369714，4.037 尚在线上。
	// 先把 ATR 舍入到 0.0013 会把保护线抬到 4.0371，虚构 ATR+MA60 共振。
	row := evaluatePositionExit(positionExitInput{
		position: model.Position{UserID: 12027, Market: "cn", BuyPrice: 4, PeakPrice: 4.041},
		quote:    freshExitQuote(now, 4.037, 4.037, 4.037), barRows: bars, now: now, session: model.PositionExitSessionIntraday,
	}, defaultPositionExitParams)
	if hasPositionExitSignal(secondRowSignals(row), "atr14_break", 0) || row.Level == model.PositionExitLevelUrgent {
		t.Fatalf("ATR 不得因中间舍入产生假跌破和风险升级：ATR=%v line=%v level=%s signals=%s", row.ATR14, row.ATRLine, row.Level, row.SignalsJSON)
	}
}

func TestMySQLPositionExitStateAndLatest(t *testing.T) {
	setupMySQLReviewDB(t, &model.Position{}, &model.PositionTrade{}, &model.PortfolioAccount{},
		&model.DailyBar{}, &model.TradingCalendar{}, &model.AlertRule{}, &model.AlertEvent{},
		&model.SellReview{}, &model.PositionCorpAdjust{}, &model.CorporateAction{}, &model.PositionExitAssessment{})
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	p := seedHoldingWithPeak(t, 12028, "600901", "ATR 数据库存储", 4, 100, 4.041, "2026-07-01")
	bars := exitTestBars(now, 4.039, 4.0397, 4.0384, 70)
	bars[len(bars)-1].High = 4.0401
	seedExitReviewBars(t, bars)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: "2026-08-10", IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionExitAssessmentService{}
	for i := 0; i < 5; i++ {
		quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(now, 4.037, 4.037, 4.037)}
		if _, err := svc.EvaluateUserWithSnapshot(context.Background(), p.UserID, []model.Position{*p}, quotes, model.PositionExitSessionIntraday, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var rows []model.PositionExitAssessment
	if err := common.DB.Where("position_id = ?", p.ID).Order("id").Find(&rows).Error; err != nil || len(rows) == 0 {
		t.Fatalf("读取评估：rows=%v err=%v", rows, err)
	}
	crossings := 0
	for _, row := range rows {
		if row.ATRLine != row.QuotePrice || row.ATRState != "below" {
			t.Fatalf("数据库四位数值相等时，仍必须保存未舍入计算的在线下状态：%+v", row)
		}
		if hasPositionExitSignal(secondRowSignals(row), "atr14_break", 0) {
			crossings++
		}
	}
	if crossings != 1 {
		t.Fatalf("存储舍入不应导致持续线下重复穿越，得到 %d 次", crossings)
	}
	// 相同 evaluated_at 多条历史按 ID 决定最新，不能回退到旧的匹配事实。
	latestRow := rows[len(rows)-1]
	latestRow.ID, latestRow.EventKey = 0, "mysql-latest-tied-time"
	latestRow.PrimaryReason = "同时间的最新事实"
	if err := common.DB.Create(&latestRow).Error; err != nil {
		t.Fatal(err)
	}
	latest, err := LatestPositionExitAssessments(context.Background(), p.UserID, []int64{p.ID})
	if err != nil || latest[p.ID].ID != latestRow.ID {
		t.Fatalf("MySQL 最新评估选择错误：latest=%+v err=%v", latest, err)
	}
	if err := common.DB.Model(p).Update("buy_price", 3).Error; err != nil {
		t.Fatal(err)
	}
	latest, err = LatestPositionExitAssessments(context.Background(), p.UserID, []int64{p.ID})
	if err != nil || len(latest) != 0 {
		t.Fatalf("变更后的持仓不能复用旧评估：latest=%+v err=%v", latest, err)
	}
	if _, err := PositionExitAssessmentByID(context.Background(), p.UserID, p.ID, latestRow.ID); err != nil {
		t.Fatalf("旧事实的精确历史入口应保持可读：%v", err)
	}
}

func TestTodoProjectsExitRiskFromOtherRealAccounts(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 12029, "600901", "第二账户的紧急风险", 10, 100, 12, "2026-07-01")
	if _, err := ResolvePortfolioAccount(p.UserID, 0, model.PortfolioKindReal); err != nil {
		t.Fatal(err)
	}
	account := model.PortfolioAccount{UserID: p.UserID, Name: "第二个真实账户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	p.PlanStopLoss = 9
	if err := common.DB.Model(p).Update("plan_stop_loss", p.PlanStopLoss).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row := evaluatePositionExit(positionExitInput{position: *p, quote: freshExitQuote(now, 8, 8, 8), now: now, session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
	if inserted, _, err := persistPositionExitAssessment(context.Background(), &row); err != nil || !inserted {
		t.Fatalf("第二个账户的评估应先成功保存：%v %v", inserted, err)
	}
	result, err := todoTestService(true).BuildInbox(t.Context(), p.UserID, TodoListOptions{Scope: TodoScopeLedger, Status: TodoStatusNeedsAction})
	if err != nil || result.Total != 1 || result.Items[0].Kind != TodoKindPositionExit {
		t.Fatalf("非默认真实账户的统一卖出风险也必须出现在待办：result=%+v err=%v", result, err)
	}
}

func TestPositionExitNotificationKeepsAccountRoute(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 12030, "600901", "另一账户的通知", 10, 100, 12, "2026-07-01")
	if _, err := ResolvePortfolioAccount(p.UserID, 0, model.PortfolioKindReal); err != nil {
		t.Fatal(err)
	}
	account := model.PortfolioAccount{UserID: p.UserID, Name: "通知所属真实账户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(p).Updates(map[string]any{"account_id": account.ID, "plan_stop_loss": 9}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.UserPreference{UserID: p.UserID, EnableNotify: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.NotifyChannel{UserID: p.UserID, Kind: model.NotifyKindWebhook, Name: "本机记录通知", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	notifier := &recordingAlertNotifier{userID: p.UserID}
	svc := &PositionExitAssessmentService{notify: notifier}
	now := time.Date(2026, 8, 11, 14, 0, 0, 0, time.Local)
	quotes := map[string]FreshQuoteResult{QuoteKey(p.Market, p.Symbol): freshExitQuote(now, 8, 8, 8)}
	if created, err := svc.EvaluateUserWithSnapshot(t.Context(), p.UserID, []model.Position{*p}, quotes, model.PositionExitSessionIntraday, now); err != nil || created != 1 {
		t.Fatalf("应成功保存一笔当前风险：created=%d err=%v", created, err)
	}
	var row model.PositionExitAssessment
	if err := common.DB.Where("position_id = ?", p.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/positions?position_id=%d&assessment_id=%d&account_id=%d", p.ID, row.ID, account.ID)
	if notifier.calls.Load() != 1 || notifier.lastMessage.Route != want {
		t.Fatalf("通知必须携带所属账户而非落入默认账户：calls=%d route=%q want=%q", notifier.calls.Load(), notifier.lastMessage.Route, want)
	}
	if len(notifier.lastMessage.BrowserEvents) != 1 || notifier.lastMessage.BrowserEvents[0].Route != want {
		t.Fatalf("浏览器通知也应使用完整账户深链：%+v", notifier.lastMessage.BrowserEvents)
	}
}
