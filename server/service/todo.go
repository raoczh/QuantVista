package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TodoService 今日待办/待复盘聚合：把分散在各处的「今天该看的」汇成一张清单，
// 不生成新数据，只聚合已有信号——命中的条件提醒、需复盘的短线推荐、需复盘的持仓、
// 到期待复盘的投资逻辑卡。与全局理念一致：不主动推送，查询即提示。
type TodoService struct {
	alert    *AlertService
	position *PositionService
	thesis   *ThesisService
}

func NewTodoService(alert *AlertService, position *PositionService, thesis *ThesisService) *TodoService {
	return &TodoService{alert: alert, position: position, thesis: thesis}
}

// 待办类型。
const (
	TodoKindAlert         = "alert"          // 条件提醒命中
	TodoKindRecReview     = "rec_review"     // 短线推荐触发止盈/止损/过期，需复盘
	TodoKindPositionShort = "position_short" // 短线持仓持有超阈值，需复盘
	TodoKindPositionLong  = "position_long"  // 长线持仓持有较久，建议定期复盘
	TodoKindThesisDue     = "thesis_due"     // 投资逻辑卡到期待复盘
	TodoKindStopLoss      = "stop_loss"      // 持仓现价接近/跌破计划止损
	TodoKindCorpAdjust    = "corp_adjust"    // 除权除息待确认折算（B8，不确认账面盈亏就是错的）
	TodoKindIpo           = "ipo"            // 今日可申购新股/可转债（B9，不依赖持仓）
	TodoKindSellReview    = "sell_review"    // 持仓命中利空事件，需卖出复核（D16）
)

// 待办范围（D18）。用户的原话：「现在提醒的是没什么用的」——噪音主体是推荐复盘
// （AI 推过就追踪，**用户没买也天天提示**）。今日待办因此默认只显示与**我的账本**
// 有关的条目，其余按范围分流到各自的页面。
//
// **数据一条不删**：范围只是消费出口的过滤器，rec_review 仍照常生成，
// 只是搬到推荐追踪页自己的区域去显示（scope=research）。
const (
	TodoScopeLedger   = "ledger"   // 与我的账本有关：持仓、除权折算、卖出复核、持仓标的的提醒/逻辑卡
	TodoScopeResearch = "research" // 研究跟踪：推荐复盘、非持仓标的的提醒与逻辑卡
	TodoScopeMarket   = "market"   // 全市场机会：打新（与我买没买无关）
	TodoScopeAll      = "all"      // 全部（显式请求才给）
)

// validTodoScope 归一并校验范围参数，空串取默认 ledger。
func validTodoScope(scope string) (string, error) {
	switch scope {
	case "", TodoScopeLedger:
		return TodoScopeLedger, nil
	case TodoScopeResearch, TodoScopeMarket, TodoScopeAll:
		return scope, nil
	}
	return "", errors.New("非法的待办范围（ledger / research / market / all）")
}

// longHoldReviewDays 长线持仓超过该交易日数提示定期复盘。
const longHoldReviewDays = 60

// TodoItem 一条待办。RefType/RefID 供前端一键跳转到对应页面处理。
type TodoItem struct {
	Kind     string     `json:"kind"`
	Scope    string     `json:"scope"`    // ledger / research / market（D18 消费出口分流）
	Priority int        `json:"priority"` // 越小越紧急，用于排序
	Symbol   string     `json:"symbol"`
	Market   string     `json:"market"`
	Name     string     `json:"name"`
	Title    string     `json:"title"`
	Detail   string     `json:"detail"`
	RefID    int64      `json:"ref_id"`
	RefType  string     `json:"ref_type"` // alerts / recommendations / positions
	DeepLink string     `json:"deep_link,omitempty"`
	Time     *time.Time `json:"time"`
}

// TodoResult 聚合结果 + 分类计数。Complete=false 表示至少一个数据块读取失败，
// 清单可能不完整——前端不得据此显示「一切都在轨道上」，须提示状态不明。
//
// **计数口径（D18）**：Total/Alerts/Reviews 恒为**过滤后**的数字（所见即所计）；
// ScopeCounts 给出各范围的全量条数，供前端提示「另有 N 条在推荐追踪页」。
type TodoResult struct {
	Date     string     `json:"date"`
	Scope    string     `json:"scope"`
	Total    int        `json:"total"`
	Alerts   int        `json:"alerts"`
	Reviews  int        `json:"reviews"` // 推荐复盘 + 持仓复盘 + 卖出复核
	Items    []TodoItem `json:"items"`
	Complete bool       `json:"complete"`         // 全部数据块读取成功才为 true
	Errors   []string   `json:"errors,omitempty"` // 读取失败的数据块说明（清单不完整的原因）

	// ScopeCounts 各范围的条数（全量，不受本次 scope 过滤影响）。
	ScopeCounts map[string]int `json:"scope_counts"`
	// Filtered 因范围过滤而未展示的条数（= 全量 − Total）。
	Filtered int `json:"filtered"`
}

var recReviewTitle = map[string]string{
	model.RecOutcomeStopLoss:   "短线推荐触发止损，需复盘",
	model.RecOutcomeTakeProfit: "短线推荐触达止盈，可考虑兑现",
	model.RecOutcomeExpired:    "短线推荐已过有效期，需复盘",
}

// Build 聚合某用户当前的待办清单。任何数据块读取失败都会登记进 Errors 并置
// Complete=false（fail-closed：不能吞错后返回空清单，让前端把「读不到止损信号」
// 显示成「一切都在轨道上」）。
//
// scope 为消费出口过滤器（D18）：默认 ledger（只看与我的账本有关的），
// **全量数据照常生成**，过滤只发生在返回前——ScopeCounts 会告诉前端别处还有多少条。
func (s *TodoService) Build(ctx context.Context, userID int64, scope string) (*TodoResult, error) {
	scope, err := validTodoScope(scope)
	if err != nil {
		return nil, err
	}
	res := &TodoResult{
		Date:     time.Now().In(time.Local).Format("2006-01-02"),
		Scope:    scope,
		Items:    []TodoItem{},
		Complete: true,
	}
	fail := func(block string, err error) {
		res.Complete = false
		res.Errors = append(res.Errors, block+"读取失败，相关待办可能缺失")
		common.SysWarn("待办聚合读取%s失败 user=%d: %v", block, userID, err)
	}

	// 我的持仓标的集合：决定「提醒/逻辑卡」这类跨域条目算不算与账本有关。
	// 读取失败时按空集处理并留痕——宁可把持仓相关的提醒错分到 research
	// （用户仍能在 all 范围看到），也不能凭空把非持仓标的说成持仓。
	heldSymbols := map[string]bool{}
	heldPositionIDs := map[int64]bool{}
	if syms, ids, err := heldPositionStateFor(userID); err == nil {
		heldSymbols, heldPositionIDs = syms, ids
	} else {
		fail("持仓标的", err)
	}

	// 1) 未读的提醒命中事件（alert_events 状态机，标记已读/忽略即完成待办）。
	if events, err := s.alert.TriggeredForUser(userID); err == nil {
		for _, e := range events {
			if isPositionAlertKind(e.Kind) && (e.PositionID == 0 || !heldPositionIDs[e.PositionID]) {
				continue // 平仓/删除后的遗留未读事件不能继续冒充当前持仓待办
			}
			t := e.TriggeredAt
			// 持仓卖出决策类（D14/D15）天生属于账本；其余按标的是否在持仓中分流。
			sc := TodoScopeResearch
			if isPositionAlertKind(e.Kind) || heldSymbols[e.Symbol] {
				sc = TodoScopeLedger
			}
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindAlert, Scope: sc, Priority: 1,
				Symbol: e.Symbol, Market: e.Market, Name: e.Name,
				Title: "条件提醒命中", Detail: e.Message,
				RefID: e.ID, RefType: "alerts", DeepLink: alertEventDeepLink(e.ID), Time: &t,
			})
		}
	} else {
		fail("提醒命中", err)
	}

	// 2) 需复盘的短线推荐（阶段6 追踪：触发止盈/止损/过期；已读的不再进清单）。
	// **scope=research**（D18）：AI 推过就追踪，用户没买也天天提示——这是旧待办的
	// 噪音主体。数据不动，只是搬到推荐追踪页自己的区域去。
	var statuses []model.RecommendationStatus
	if err := common.DB.Where("user_id = ? AND review_needed = ? AND review_ack = ?", userID, true, false).
		Order("updated_at DESC").Find(&statuses).Error; err == nil {
		for _, st := range statuses {
			title := recReviewTitle[st.Outcome]
			if title == "" {
				title = "短线推荐需复盘"
			}
			pri := 2
			if st.Outcome == model.RecOutcomeStopLoss {
				pri = 1 // 止损最紧急
			}
			// RefID=追踪状态行 id，供前端「已读」就地消项（与 alert 条目 ref_id=事件 id 同款）；
			// 跳转不依赖 ref_id（去处理仍整页跳推荐页）。
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindRecReview, Scope: TodoScopeResearch, Priority: pri,
				Symbol: st.Symbol, Market: st.Market, Name: st.Symbol,
				Title: title, Detail: recReviewDetail(st),
				RefID: st.ID, RefType: "recommendations",
			})
		}
	} else {
		fail("推荐复盘", err)
	}

	// 3) 需复盘的持仓（短线超阈值 / 长线持有较久）+ 止损计划风控（最高优先级）。
	if views, err := s.position.List(ctx, userID, model.PositionStatusHolding); err == nil {
		unknownStop := 0 // 行情非 fresh、止损状态无法判定的仓数（fail-closed：状态不明必须显式提示）
		for _, v := range views {
			if !v.QuoteOK && v.PlanStopLoss > 0 {
				unknownStop++
			}
			// 止损信号独立于复盘信号：破止损/近止损是当下要处理的风险。
			switch {
			case v.BelowStopLoss:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindStopLoss, Scope: TodoScopeLedger, Priority: 1,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "持仓已跌破计划止损",
					Detail: fmt.Sprintf("现价 %.2f 已低于计划止损 %.2f，按纪律应复核是否离场", v.CurrentPrice, v.PlanStopLoss),
					RefID:  v.ID, RefType: "positions",
				})
			case v.NearStopLoss:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindStopLoss, Scope: TodoScopeLedger, Priority: 1,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "持仓接近计划止损",
					Detail: fmt.Sprintf("现价 %.2f 距计划止损 %.2f 不足 3%%，请提前想好应对", v.CurrentPrice, v.PlanStopLoss),
					RefID:  v.ID, RefType: "positions",
				})
			}
			switch {
			case v.ShortTermReview:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindPositionShort, Scope: TodoScopeLedger, Priority: 2,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "短线持仓需复盘",
					Detail: "已持有 " + strconv.Itoa(v.HeldTradeDays) + " 交易日，建议复盘是否止盈/止损或转长线",
					RefID:  v.ID, RefType: "positions",
				})
			case v.PositionType == model.PositionTypeLongTerm && v.HeldTradeDays > longHoldReviewDays:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindPositionLong, Scope: TodoScopeLedger, Priority: 3,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "长线持仓定期复盘",
					Detail: "已持有 " + strconv.Itoa(v.HeldTradeDays) + " 交易日，建议检查长期逻辑是否仍成立",
					RefID:  v.ID, RefType: "positions",
				})
			}
		}
		// 止损待办的判定依赖当前有效行情：行情过期/失败的仓无法判「破/近止损」，
		// 状态不明必须显式提示，不能静默当作「一切正常」。
		if unknownStop > 0 {
			res.Complete = false
			res.Errors = append(res.Errors, fmt.Sprintf("%d 笔设有止损计划的持仓无当前有效行情，止损状态未知（非「未触发」）", unknownStop))
		}
	} else {
		// 止损信号来源于此块，读取失败必须留痕（静默吞错会让「破止损」待办凭空消失）。
		fail("持仓", err)
	}

	// 4) 到期待复盘的投资逻辑卡。持仓标的的逻辑卡属账本，其余属研究跟踪。
	if s.thesis != nil {
		if cards, err := s.thesis.DueForUser(userID); err == nil {
			for _, c := range cards {
				sc := TodoScopeResearch
				if heldSymbols[c.Symbol] {
					sc = TodoScopeLedger
				}
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindThesisDue, Scope: sc, Priority: 3,
					Symbol: c.Symbol, Market: c.Market, Name: c.Name,
					Title:  "投资逻辑卡到期复盘",
					Detail: "计划复盘日 " + c.NextReviewDate + "，请检查核心逻辑与失效条件是否仍成立",
					RefID:  c.ID, RefType: "thesis",
				})
			}
		} else {
			fail("逻辑卡", err)
		}
	}

	// 5) 除权除息待确认折算（B8）：先按今日除权日生成建议（幂等），再把 pending 的列出来。
	// 优先级 1——不确认的话持仓页显示的盈亏就是**错的数字**（10 转 10 后 -50%），
	// 比「接近止损」更该立刻处理。
	if _, err := GenerateCorpAdjusts(userID, res.Date); err != nil {
		fail("除权除息调整", err)
	}
	if adjusts, err := ListCorpAdjusts(userID, model.CorpAdjustPending); err == nil {
		for _, a := range adjusts {
			detail := fmt.Sprintf("%s 除权除息（%s）：数量 %g → %g 股，成本 %.4f → %.4f 元",
				a.ExDate, orSymbol(a.PlanProfile, "方案详见公告"),
				a.QtyBefore, a.QtyAfter, a.CostBefore, a.CostAfter)
			if a.CashDividend > 0 {
				detail += fmt.Sprintf("，现金分红 %.2f 元（税前）", a.CashDividend)
			}
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindCorpAdjust, Scope: TodoScopeLedger, Priority: 1,
				Symbol: a.Symbol, Market: a.Market, Name: orSymbol(a.Name, a.Symbol),
				Title:  "除权除息待确认折算",
				Detail: detail,
				RefID:  a.ID, RefType: "positions",
			})
		}
	} else {
		fail("除权除息调整", err)
	}

	// 6) 今日打新（B9）：**不依赖持仓**，全市场公开信息，人人可见。
	// scope=market（D18）——它是机会不是我的账本；默认范围不显示，
	// 但 Today 页的事件日历卡仍照常列出打新，信息一条不丢。
	var subs []model.IpoSubscription
	if err := common.DB.Where("apply_date = ?", res.Date).Order("kind, code").Find(&subs).Error; err == nil {
		for _, sub := range subs {
			label := "新股"
			extra := sub.Board
			if sub.Kind == model.IpoKindCb {
				label = "可转债"
				extra = ""
				if sub.StockName != "" {
					extra = fmt.Sprintf("正股 %s(%s)", sub.StockName, sub.StockCode)
				}
				if sub.Rating != "" {
					extra = appendSep(extra, "评级 "+sub.Rating)
				}
			} else if sub.ApplyUpper > 0 {
				extra = appendSep(extra, fmt.Sprintf("申购上限 %.0f 股", sub.ApplyUpper))
			}
			if sub.IssuePrice > 0 {
				extra = appendSep(extra, fmt.Sprintf("发行价 %.2f 元", sub.IssuePrice))
			} else {
				extra = appendSep(extra, "发行价待定")
			}
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindIpo, Scope: TodoScopeMarket, Priority: 2,
				Symbol: sub.ApplyCode, Market: "cn", Name: sub.Name,
				Title:  "今日可申购" + label,
				Detail: fmt.Sprintf("申购代码 %s，%s", sub.ApplyCode, extra),
				RefID:  sub.ID, RefType: "ipo",
			})
		}
	} else {
		fail("打新日历", err)
	}

	// 7) 卖出复核（D16）：持仓命中解禁/业绩变脸/跌破均线/龙虎榜出货/除权除息。
	// 这是本批的核心——**唯一一类「基于我的成本、告诉我该不该卖」的待办**。
	// high 严重度按优先级 1 与止损并列，其余 2。
	if reviews, err := ListSellReviews(userID, model.SellReviewStatusOpen); err == nil {
		for _, r := range reviews {
			pri := 2
			if r.Severity == model.SellReviewSeverityHigh {
				pri = 1
			}
			t := r.CreatedAt
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindSellReview, Scope: TodoScopeLedger, Priority: pri,
				Symbol: r.Symbol, Market: r.Market, Name: r.Name,
				Title:  "卖出复核 · " + r.Title,
				Detail: r.Detail,
				RefID:  r.ID, RefType: "positions", Time: &t,
			})
		}
	} else {
		fail("卖出复核", err)
	}

	// 排序：优先级升序，其次有时间者靠前、时间新者靠前。
	sort.SliceStable(res.Items, func(i, j int) bool {
		a, b := res.Items[i], res.Items[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if (a.Time != nil) != (b.Time != nil) {
			return a.Time != nil
		}
		if a.Time != nil && b.Time != nil {
			return a.Time.After(*b.Time)
		}
		return false
	})

	// 范围过滤 + 分类计数（**过滤之后才计数**：所见即所计，
	// 否则徽标显示 12 条、点进去只有 3 条，用户会以为丢了东西）。
	all := res.Items
	res.ScopeCounts = map[string]int{TodoScopeLedger: 0, TodoScopeResearch: 0, TodoScopeMarket: 0}
	for _, it := range all {
		res.ScopeCounts[it.Scope]++
	}
	kept := make([]TodoItem, 0, len(all))
	for _, it := range all {
		if scope != TodoScopeAll && it.Scope != scope {
			continue
		}
		kept = append(kept, it)
		switch it.Kind {
		case TodoKindAlert:
			res.Alerts++
		case TodoKindRecReview, TodoKindStopLoss, TodoKindPositionShort,
			TodoKindPositionLong, TodoKindThesisDue, TodoKindCorpAdjust, TodoKindSellReview:
			res.Reviews++
		}
	}
	res.Items = kept
	res.Total = len(kept)
	res.Filtered = len(all) - len(kept)
	return res, nil
}

// heldPositionStateFor 返回用户当前持仓的标的与持仓 ID 集合。
func heldPositionStateFor(userID int64) (map[string]bool, map[int64]bool, error) {
	symbols := map[string]bool{}
	ids := map[int64]bool{}
	if common.DB == nil {
		return symbols, ids, errors.New("数据库不可用")
	}
	var rows []struct {
		ID     int64
		Symbol string
	}
	if err := common.DB.Model(&model.Position{}).
		Where("user_id = ? AND status = ?", userID, model.PositionStatusHolding).
		Select("id, symbol").Find(&rows).Error; err != nil {
		return symbols, ids, err
	}
	for _, row := range rows {
		symbols[row.Symbol] = true
		ids[row.ID] = true
	}
	return symbols, ids, nil
}

func recReviewDetail(st model.RecommendationStatus) string {
	d := "当前收益 " + fmt.Sprintf("%.2f", st.ReturnPct) + "%"
	if st.Outcome == model.RecOutcomeExpired && st.ValidDays > 0 {
		d += "，已过 " + strconv.Itoa(st.ValidDays) + " 交易日有效期"
	}
	return d
}
