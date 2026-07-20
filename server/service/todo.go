package service

import (
	"context"
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
)

// longHoldReviewDays 长线持仓超过该交易日数提示定期复盘。
const longHoldReviewDays = 60

// TodoItem 一条待办。RefType/RefID 供前端一键跳转到对应页面处理。
type TodoItem struct {
	Kind     string     `json:"kind"`
	Priority int        `json:"priority"` // 越小越紧急，用于排序
	Symbol   string     `json:"symbol"`
	Market   string     `json:"market"`
	Name     string     `json:"name"`
	Title    string     `json:"title"`
	Detail   string     `json:"detail"`
	RefID    int64      `json:"ref_id"`
	RefType  string     `json:"ref_type"` // alerts / recommendations / positions
	Time     *time.Time `json:"time"`
}

// TodoResult 聚合结果 + 分类计数。Complete=false 表示至少一个数据块读取失败，
// 清单可能不完整——前端不得据此显示「一切都在轨道上」，须提示状态不明。
type TodoResult struct {
	Date     string     `json:"date"`
	Total    int        `json:"total"`
	Alerts   int        `json:"alerts"`
	Reviews  int        `json:"reviews"` // 推荐复盘 + 持仓复盘
	Items    []TodoItem `json:"items"`
	Complete bool       `json:"complete"`         // 全部数据块读取成功才为 true
	Errors   []string   `json:"errors,omitempty"` // 读取失败的数据块说明（清单不完整的原因）
}

var recReviewTitle = map[string]string{
	model.RecOutcomeStopLoss:   "短线推荐触发止损，需复盘",
	model.RecOutcomeTakeProfit: "短线推荐触达止盈，可考虑兑现",
	model.RecOutcomeExpired:    "短线推荐已过有效期，需复盘",
}

// Build 聚合某用户当前的待办清单。任何数据块读取失败都会登记进 Errors 并置
// Complete=false（fail-closed：不能吞错后返回空清单，让前端把「读不到止损信号」
// 显示成「一切都在轨道上」）。
func (s *TodoService) Build(ctx context.Context, userID int64) (*TodoResult, error) {
	res := &TodoResult{
		Date:     time.Now().In(time.Local).Format("2006-01-02"),
		Items:    []TodoItem{},
		Complete: true,
	}
	fail := func(block string, err error) {
		res.Complete = false
		res.Errors = append(res.Errors, block+"读取失败，相关待办可能缺失")
		common.SysWarn("待办聚合读取%s失败 user=%d: %v", block, userID, err)
	}

	// 1) 未读的提醒命中事件（alert_events 状态机，标记已读/忽略即完成待办）。
	if events, err := s.alert.TriggeredForUser(userID); err == nil {
		for _, e := range events {
			t := e.TriggeredAt
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindAlert, Priority: 1,
				Symbol: e.Symbol, Market: e.Market, Name: e.Name,
				Title: "条件提醒命中", Detail: e.Message,
				RefID: e.ID, RefType: "alerts", Time: &t,
			})
			res.Alerts++
		}
	} else {
		fail("提醒命中", err)
	}

	// 2) 需复盘的短线推荐（阶段6 追踪：触发止盈/止损/过期；已读的不再进清单）。
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
				Kind: TodoKindRecReview, Priority: pri,
				Symbol: st.Symbol, Market: st.Market, Name: st.Symbol,
				Title: title, Detail: recReviewDetail(st),
				RefID: st.ID, RefType: "recommendations",
			})
			res.Reviews++
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
					Kind: TodoKindStopLoss, Priority: 1,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "持仓已跌破计划止损",
					Detail: fmt.Sprintf("现价 %.2f 已低于计划止损 %.2f，按纪律应复核是否离场", v.CurrentPrice, v.PlanStopLoss),
					RefID:  v.ID, RefType: "positions",
				})
				res.Reviews++
			case v.NearStopLoss:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindStopLoss, Priority: 1,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "持仓接近计划止损",
					Detail: fmt.Sprintf("现价 %.2f 距计划止损 %.2f 不足 3%%，请提前想好应对", v.CurrentPrice, v.PlanStopLoss),
					RefID:  v.ID, RefType: "positions",
				})
				res.Reviews++
			}
			switch {
			case v.ShortTermReview:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindPositionShort, Priority: 2,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "短线持仓需复盘",
					Detail: "已持有 " + strconv.Itoa(v.HeldTradeDays) + " 交易日，建议复盘是否止盈/止损或转长线",
					RefID:  v.ID, RefType: "positions",
				})
				res.Reviews++
			case v.PositionType == model.PositionTypeLongTerm && v.HeldTradeDays > longHoldReviewDays:
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindPositionLong, Priority: 3,
					Symbol: v.Symbol, Market: v.Market, Name: v.Name,
					Title:  "长线持仓定期复盘",
					Detail: "已持有 " + strconv.Itoa(v.HeldTradeDays) + " 交易日，建议检查长期逻辑是否仍成立",
					RefID:  v.ID, RefType: "positions",
				})
				res.Reviews++
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

	// 4) 到期待复盘的投资逻辑卡。
	if s.thesis != nil {
		if cards, err := s.thesis.DueForUser(userID); err == nil {
			for _, c := range cards {
				res.Items = append(res.Items, TodoItem{
					Kind: TodoKindThesisDue, Priority: 3,
					Symbol: c.Symbol, Market: c.Market, Name: c.Name,
					Title:  "投资逻辑卡到期复盘",
					Detail: "计划复盘日 " + c.NextReviewDate + "，请检查核心逻辑与失效条件是否仍成立",
					RefID:  c.ID, RefType: "thesis",
				})
				res.Reviews++
			}
		} else {
			fail("逻辑卡", err)
		}
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
	res.Total = len(res.Items)
	return res, nil
}

func recReviewDetail(st model.RecommendationStatus) string {
	d := "当前收益 " + fmt.Sprintf("%.2f", st.ReturnPct) + "%"
	if st.Outcome == model.RecOutcomeExpired && st.ValidDays > 0 {
		d += "，已过 " + strconv.Itoa(st.ValidDays) + " 交易日有效期"
	}
	return d
}
