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

// TodoResult 聚合结果 + 分类计数。
type TodoResult struct {
	Date    string     `json:"date"`
	Total   int        `json:"total"`
	Alerts  int        `json:"alerts"`
	Reviews int        `json:"reviews"` // 推荐复盘 + 持仓复盘
	Items   []TodoItem `json:"items"`
}

var recReviewTitle = map[string]string{
	model.RecOutcomeStopLoss:   "短线推荐触发止损，需复盘",
	model.RecOutcomeTakeProfit: "短线推荐触达止盈，可考虑兑现",
	model.RecOutcomeExpired:    "短线推荐已过有效期，需复盘",
}

// Build 聚合某用户当前的待办清单。
func (s *TodoService) Build(ctx context.Context, userID int64) (*TodoResult, error) {
	res := &TodoResult{
		Date:  time.Now().In(time.Local).Format("2006-01-02"),
		Items: []TodoItem{},
	}

	// 1) 命中的条件提醒。
	if alerts, err := s.alert.TriggeredForUser(userID); err == nil {
		for _, a := range alerts {
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindAlert, Priority: 1,
				Symbol: a.Symbol, Market: a.Market, Name: a.Name,
				Title: "条件提醒命中", Detail: a.TriggerMsg,
				RefID: a.ID, RefType: "alerts", Time: a.TriggeredAt,
			})
			res.Alerts++
		}
	}

	// 2) 需复盘的短线推荐（阶段6 追踪：触发止盈/止损/过期）。
	var statuses []model.RecommendationStatus
	if err := common.DB.Where("user_id = ? AND review_needed = ?", userID, true).
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
			res.Items = append(res.Items, TodoItem{
				Kind: TodoKindRecReview, Priority: pri,
				Symbol: st.Symbol, Market: st.Market, Name: st.Symbol,
				Title: title, Detail: recReviewDetail(st),
				RefID: st.BatchID, RefType: "recommendations",
			})
			res.Reviews++
		}
	}

	// 3) 需复盘的持仓（短线超阈值 / 长线持有较久）。
	if views, err := s.position.List(ctx, userID, model.PositionStatusHolding); err == nil {
		for _, v := range views {
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
