package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// TestTodoBuild 聚合命中提醒 + 推荐复盘 + 持仓复盘，验证计数、排序与用户隔离。
func TestTodoBuild(t *testing.T) {
	setupTestDB(t)
	// 同进程共用一个内存库：别的用例落的除权建议/卖出复核/打新都会混进本用例的计数，
	// 统一走 cleanCorpTables 清干净（它已覆盖 positions/alert_*/sell_reviews/corp 系列）。
	cleanCorpTables(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")

	// 未读的提醒命中事件（用户1）——批次 H 起待办以 alert_events unread 为准。
	now := time.Now()
	common.DB.Create(&model.AlertEvent{RuleID: 1, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Message: "当日最高触及 ≥ 11", TriggeredAt: now, Status: model.AlertEventUnread})
	common.DB.Create(&model.AlertEvent{RuleID: 2, UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindVolumeSurge, Message: "当日量达 20 日均量的 2.50 倍", TriggeredAt: now, Status: model.AlertEventUnread})
	// 已读事件不进待办。
	common.DB.Create(&model.AlertEvent{RuleID: 3, UserID: 1, Symbol: "000002", Market: "cn", Name: "万科A",
		Kind: model.AlertKindPrice, Message: "已处理过的命中", TriggeredAt: now, Status: model.AlertEventRead})
	// 他人事件（不应出现）。
	common.DB.Create(&model.AlertEvent{RuleID: 4, UserID: 2, Symbol: "000001", Market: "cn",
		Kind: model.AlertKindPrice, Message: "他人命中", TriggeredAt: now, Status: model.AlertEventUnread})

	// 需复盘的短线推荐：止损（priority 1）+ 过期（priority 2）。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 1, BatchID: 10, UserID: 1, Symbol: "600519",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeStopLoss, ReviewNeeded: true, ReturnPct: -8})
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 2, BatchID: 11, UserID: 1, Symbol: "000002",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReturnPct: 1, ValidDays: 5})
	// 不需复盘的（进行中）不计入。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 3, BatchID: 12, UserID: 1, Symbol: "600004",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeActive, ReviewNeeded: false})
	// 已读的复盘提示不再进清单（review_ack 人工标记，追踪刷新不覆盖）。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 4, BatchID: 13, UserID: 1, Symbol: "600005",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReviewAck: true})

	svc := NewTodoService(&AlertService{}, &PositionService{market: nil}, nil)
	// position.List 需要 market 富化；这里无持仓，List 返回空即可（跳过持仓分支）。
	// 用 all 范围断言全量聚合口径；D18 的范围过滤另有 TestTodoScopeFilter 专测。
	res, err := svc.Build(context.Background(), 1, TodoScopeAll)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if res.Alerts != 2 {
		t.Fatalf("命中提醒应为 2（未读事件，已读不计），得到 %d", res.Alerts)
	}
	if res.Reviews != 2 {
		t.Fatalf("推荐复盘应为 2，得到 %d", res.Reviews)
	}
	if res.Total != 4 {
		t.Fatalf("待办合计应为 4，得到 %d", res.Total)
	}
	// 新收件箱排序：需处理项在仅知晓的普通提醒之前，顺序由服务端统一决定。
	if res.Items[0].Status != TodoStatusNeedsAction || res.Items[len(res.Items)-1].Status != TodoStatusAwareness {
		t.Fatalf("需处理项应排在仅知晓项之前: %+v", res.Items)
	}
	// alert 条目的 RefID 应为事件 id（供前端标记已读/忽略）；rec_review 条目的
	// RefID 应为追踪状态行 id（供前端「已读」消项）。
	for _, it := range res.Items {
		if it.Kind == TodoKindAlert && it.RefID == 0 {
			t.Fatalf("alert 待办应携带事件 id: %+v", it)
		}
		if it.Kind == TodoKindRecReview && it.RefID == 0 {
			t.Fatalf("rec_review 待办应携带追踪状态 id: %+v", it)
		}
	}

	// AckReview 标记已读后从清单消失；他人不可标记。
	var st model.RecommendationStatus
	if err := common.DB.Where("recommendation_id = ?", 1).First(&st).Error; err != nil {
		t.Fatalf("读取追踪状态失败: %v", err)
	}
	tracking := NewTrackingService(nil)
	if err := tracking.AckReview(2, st.ID); err == nil {
		t.Fatalf("他人标记已读应被拒绝")
	}
	if err := tracking.AckReview(1, st.ID); err != nil {
		t.Fatalf("AckReview 失败: %v", err)
	}
	resAck, _ := svc.Build(context.Background(), 1, TodoScopeAll)
	if resAck.Reviews != 1 {
		t.Fatalf("已读后推荐复盘应剩 1，得到 %d", resAck.Reviews)
	}

	// 用户隔离：用户2 只见自己的 1 条命中提醒。
	res2, _ := svc.Build(context.Background(), 2, TodoScopeAll)
	if res2.Total != 1 || res2.Alerts != 1 {
		t.Fatalf("用户2 应只见 1 条命中提醒，得到 total=%d alerts=%d", res2.Total, res2.Alerts)
	}
}

// TestTodoScopeFilter D18 待办改造：默认只显示与我的账本有关的条目，
// 推荐复盘归 research（挪到推荐追踪页）、打新归 market，分类计数按过滤后计算。
func TestTodoScopeFilter(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	common.DB.Exec("DELETE FROM recommendation_statuses")
	common.DB.Exec("DELETE FROM sell_reviews")
	today := time.Now().In(time.Local).Format("2006-01-02")
	now := time.Now()

	// 我持有 600000，没有持有 600519。
	p := seedHoldingWithPeak(t, 1, "600000", "浦发银行", 10, 1000, 10, today)

	// ① 持仓标的的提醒命中 → ledger；② 非持仓标的的提醒命中 → research；
	// ③ 持仓类 kind 的提醒（symbol 恰好也在持仓里）→ ledger。
	common.DB.Create(&model.AlertEvent{RuleID: 1, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPrice, Message: "到价", TradeDate: today, TriggeredAt: now, Status: model.AlertEventUnread})
	common.DB.Create(&model.AlertEvent{RuleID: 2, UserID: 1, Symbol: "600519", Market: "cn", Name: "贵州茅台",
		Kind: model.AlertKindPrice, Message: "到价", TradeDate: today, TriggeredAt: now, Status: model.AlertEventUnread})
	common.DB.Create(&model.AlertEvent{RuleID: 3, UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Kind: model.AlertKindPeakDrawdown, Message: "自持仓期最高回撤 20.00%", TradeDate: today,
		PositionID: p.ID, TriggeredAt: now, Status: model.AlertEventUnread})
	// 推荐复盘（噪音主体）→ research。
	common.DB.Create(&model.RecommendationStatus{RecommendationID: 1, BatchID: 10, UserID: 1, Symbol: "000001",
		Type: model.RecTypeShortTerm, Outcome: model.RecOutcomeExpired, ReviewNeeded: true, ReturnPct: 1, ValidDays: 5})
	// 打新 → market。
	common.DB.Create(&model.IpoSubscription{Kind: model.IpoKindStock, Code: "301000", Name: "新股A",
		ApplyCode: "301000", ApplyDate: today, IssuePrice: 20})
	// 卖出复核 → ledger。
	common.DB.Create(&model.SellReview{UserID: 1, PositionID: p.ID, Symbol: "600000", Market: "cn",
		Name: "浦发银行", Trigger: model.SellReviewLift, TradeDate: today,
		Severity: model.SellReviewSeverityMed, Title: "限售解禁临近", Detail: "解禁 3000 万股",
		Status: model.SellReviewStatusOpen})

	// 有持仓时 PositionService 必须有可用的 market（零适配器 Manager 让行情逐源失败，
	// 走真实的 fail-closed 分支而不是 nil 崩溃）。
	svc := NewTodoService(&AlertService{},
		NewPositionService(NewMarketService(datasource.NewManagerWithAdapters())), nil)

	// 默认（ledger）：2 条提醒 + 1 条卖出复核；推荐复盘与打新不在其中。
	led, err := svc.Build(context.Background(), 1, "")
	if err != nil {
		t.Fatalf("待办聚合失败: %v", err)
	}
	if led.Scope != TodoScopeLedger {
		t.Fatalf("默认范围应为 ledger，得到 %s", led.Scope)
	}
	if led.Total != 3 {
		t.Fatalf("账本范围应有 3 条（2 提醒 + 1 卖出复核），得到 %d: %+v", led.Total, led.Items)
	}
	for _, it := range led.Items {
		if it.Kind == TodoKindRecReview {
			t.Fatal("推荐复盘不得出现在默认待办里（已挪到推荐追踪页）")
		}
		if it.Kind == TodoKindIpo {
			t.Fatal("打新属全市场机会，不在账本范围")
		}
		if it.Symbol == "600519" {
			t.Fatal("非持仓标的的提醒不属账本范围")
		}
	}
	// 计数按过滤后算：所见即所计（否则徽标 6 条、点进去 3 条）。
	if led.Alerts != 2 || led.Reviews != 1 {
		t.Fatalf("分类计数应按过滤后计算，得到 alerts=%d reviews=%d", led.Alerts, led.Reviews)
	}
	if led.Filtered != 3 {
		t.Fatalf("被过滤条数应为 3（推荐复盘 + 打新 + 非持仓提醒），得到 %d，status=%+v source=%+v scope=%+v items=%+v",
			led.Filtered, led.StatusCounts, led.SourceCounts, led.ScopeCounts, led.Items)
	}
	if led.ScopeCounts[TodoScopeResearch] != 2 || led.ScopeCounts[TodoScopeMarket] != 1 {
		t.Fatalf("ScopeCounts 应给出别处的条数（供前端提示）: %+v", led.ScopeCounts)
	}

	// research：推荐追踪页的复盘区取这一份。
	res, _ := svc.Build(context.Background(), 1, TodoScopeResearch)
	if res.Total != 2 {
		t.Fatalf("research 范围应有 2 条（推荐复盘 + 非持仓提醒），得到 %d", res.Total)
	}
	var hasRec bool
	for _, it := range res.Items {
		if it.Kind == TodoKindRecReview {
			hasRec = true
		}
	}
	if !hasRec {
		t.Fatal("推荐复盘数据必须仍在（只改消费出口，不删数据）")
	}

	// all：全量 6 条。
	all, _ := svc.Build(context.Background(), 1, TodoScopeAll)
	if all.Total != 6 || all.Filtered != 0 {
		t.Fatalf("all 范围应为全量 6 条且过滤 0，得到 total=%d filtered=%d", all.Total, all.Filtered)
	}

	// 非法范围被拒。
	if _, err := svc.Build(context.Background(), 1, "whatever"); err == nil {
		t.Fatal("非法范围应被拒绝")
	}

	// 用户隔离：用户 2 什么都看不到。
	other, _ := svc.Build(context.Background(), 2, TodoScopeAll)
	for _, it := range other.Items {
		if it.Kind != TodoKindIpo {
			t.Fatalf("用户 2 只应看到全市场打新，得到 %+v", it)
		}
	}
}

func TestTodoUsesUnifiedPositionExitFactAndKeepsHandledState(t *testing.T) {
	setupTestDB(t)
	cleanCorpTables(t)
	common.DB.Exec("DELETE FROM position_exit_assessments WHERE user_id = ?", 906)
	common.DB.Exec("DELETE FROM todo_inbox_states WHERE user_id = ?", 906)
	today := time.Now().In(time.Local).Format("2006-01-02")
	p := seedHoldingWithPeak(t, 906, "600906", "统一评估股", 10, 100, 12, today)
	common.DB.Create(&model.AlertEvent{RuleID: 9061, UserID: 906, PositionID: p.ID, Symbol: p.Symbol, Market: "cn", Kind: model.AlertKindCostDrawdown, Message: "底层成本回撤", TradeDate: today, TriggeredAt: time.Now(), Status: model.AlertEventUnread})
	common.DB.Create(&model.SellReview{UserID: 906, PositionID: p.ID, Symbol: p.Symbol, Market: "cn", Name: p.Name, Trigger: model.SellReviewLift, TradeDate: today, Severity: model.SellReviewSeverityHigh, Title: "底层解禁事实", Detail: "底层事实", Status: model.SellReviewStatusOpen})
	assessment := &model.PositionExitAssessment{UserID: 906, PositionID: p.ID, Symbol: p.Symbol, Market: "cn", Name: p.Name,
		TradeDate: today, Session: model.PositionExitSessionIntraday, EvaluatedAt: time.Now(), Level: model.PositionExitLevelReview,
		PrimarySignal: model.AlertKindCostDrawdown, PrimaryReason: "成本回撤达到用户阈值", NextAction: "今天完成复核",
		DataStatus: model.PositionExitDataReady, ShouldTodo: true, Version: model.PositionExitAssessmentVersion,
		FactHash: "todo-review-fact", EventKey: "todo-review-event"}
	if err := common.DB.Create(assessment).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewTodoService(&AlertService{}, NewPositionService(NewMarketService(datasource.NewManagerWithAdapters())), nil)
	res, err := svc.Build(context.Background(), 906, TodoScopeLedger)
	if err != nil || res.Total != 1 || res.Items[0].Kind != TodoKindPositionExit {
		t.Fatalf("Today 必须只投影统一评估，不重复底层 Alert/SellReview: total=%d err=%v items=%+v", res.Total, err, res.Items)
	}
	child := res.Items[0].Children[0]
	if err := svc.ApplyInboxAction(906, TodoActionRequest{Action: TodoActionRead, Items: []TodoSourceRef{{SourceKind: child.SourceKind, SourceID: child.SourceID, SourceVersion: child.SourceVersion}}}); err != nil {
		t.Fatalf("统一评估待办应可完成: %v", err)
	}
	open, _ := svc.Build(context.Background(), 906, TodoScopeLedger)
	if open.Total != 0 {
		t.Fatalf("已处理的同一评估事实不得被重复拉回: %+v", open.Items)
	}
	closeSnapshot := *assessment
	closeSnapshot.ID = 0
	closeSnapshot.Session = model.PositionExitSessionClose
	closeSnapshot.EventKey = "todo-review-close-event"
	closeSnapshot.EvaluatedAt = time.Now().Add(500 * time.Millisecond)
	if err := common.DB.Create(&closeSnapshot).Error; err != nil {
		t.Fatal(err)
	}
	closedAgain, _ := svc.Build(context.Background(), 906, TodoScopeLedger)
	if closedAgain.Total != 0 {
		t.Fatalf("盘后追加的同交易日同事实不得让已处理 Todo 回流: %+v", closedAgain.Items)
	}
	urgent := *assessment
	urgent.ID = 0
	urgent.Level = model.PositionExitLevelUrgent
	urgent.PrimarySignal = "plan_stop"
	urgent.PrimaryReason = "触达计划止损"
	urgent.FactHash = "todo-urgent-fact"
	urgent.EventKey = "todo-urgent-event"
	urgent.PreviousID = closeSnapshot.ID
	urgent.IsUpgrade = true
	urgent.EvaluatedAt = time.Now().Add(time.Second)
	if err := common.DB.Create(&urgent).Error; err != nil {
		t.Fatal(err)
	}
	upgraded, _ := svc.Build(context.Background(), 906, TodoScopeLedger)
	if upgraded.Total != 1 || upgraded.Items[0].Severity != "critical" {
		t.Fatalf("review→urgent 新事实应再次出现且高优先级: %+v", upgraded.Items)
	}
}
