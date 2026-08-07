package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	TodoStatusOpen        = "open"
	TodoStatusNeedsAction = "needs_action"
	TodoStatusAwareness   = "awareness"
	TodoStatusCompleted   = "completed"
	TodoStatusAll         = "all"

	TodoActionRead      = "read"
	TodoActionSnooze    = "snooze"
	TodoActionMuteToday = "mute_today"

	todoHistoryDefaultDays = 30
	todoHistoryMaxDays     = 90
)

// TodoChild 保留合并组内每一条原始业务事实及其深链，组级降噪不会丢来源。
type TodoChild struct {
	Kind          string     `json:"kind"`
	SourceKind    string     `json:"source_kind"`
	SourceID      int64      `json:"source_id"`
	SourceVersion string     `json:"source_version"`
	SourceLabel   string     `json:"source_label"`
	Title         string     `json:"title"`
	Detail        string     `json:"detail"`
	Severity      string     `json:"severity"`
	RefID         int64      `json:"ref_id"`
	RefType       string     `json:"ref_type"`
	DeepLink      string     `json:"deep_link,omitempty"`
	Time          *time.Time `json:"time"`
	DueAt         *time.Time `json:"due_at,omitempty"`
	Read          bool       `json:"read"`
	SnoozedUntil  *time.Time `json:"snoozed_until,omitempty"`
	CanComplete   bool       `json:"can_complete"`
}

type TodoListOptions struct {
	Scope       string
	Status      string
	Source      string
	Page        int
	PageSize    int
	HistoryDays int
	Limit       int
}

type TodoSourceRef struct {
	SourceKind    string `json:"source_kind"`
	SourceID      int64  `json:"source_id"`
	SourceVersion string `json:"source_version"`
}

type TodoActionRequest struct {
	Action string          `json:"action"`
	Items  []TodoSourceRef `json:"items"`
}

// Build 保持旧调用契约：返回指定 scope 下尚未完成的“需处理 + 仅知晓”。
func (s *TodoService) Build(ctx context.Context, userID int64, scope string) (*TodoResult, error) {
	return s.BuildInbox(ctx, userID, TodoListOptions{Scope: scope, Status: TodoStatusOpen})
}

func normalizeTodoOptions(opts TodoListOptions) (TodoListOptions, error) {
	scope, err := validTodoScope(strings.TrimSpace(opts.Scope))
	if err != nil {
		return opts, err
	}
	opts.Scope = scope
	opts.Status = strings.TrimSpace(opts.Status)
	if opts.Status == "" {
		opts.Status = TodoStatusOpen
	}
	switch opts.Status {
	case TodoStatusOpen, TodoStatusNeedsAction, TodoStatusAwareness, TodoStatusCompleted, TodoStatusAll:
	default:
		return opts, errors.New("非法的收件箱状态")
	}
	opts.Source = strings.TrimSpace(opts.Source)
	if opts.Page <= 0 {
		opts.Page = 1
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 20
	}
	if opts.PageSize > 100 {
		opts.PageSize = 100
	}
	if opts.HistoryDays <= 0 {
		opts.HistoryDays = todoHistoryDefaultDays
	}
	if opts.HistoryDays > todoHistoryMaxDays {
		opts.HistoryDays = todoHistoryMaxDays
	}
	if opts.Limit < 0 || opts.Limit > 100 {
		return opts, errors.New("非法的返回条数")
	}
	return opts, nil
}

// BuildInbox 在现有 TodoService 上构建统一收件箱投影。所有正文均即时来自原业务表；
// InboxState 只影响展示状态，不成为提醒、持仓或任务的第二事实来源。
func (s *TodoService) BuildInbox(ctx context.Context, userID int64, options TodoListOptions) (*TodoResult, error) {
	opts, err := normalizeTodoOptions(options)
	if err != nil {
		return nil, err
	}
	res, err := s.buildActive(ctx, userID, TodoScopeAll)
	if err != nil {
		return nil, err
	}
	res.Scope, res.Status, res.Source = opts.Scope, opts.Status, opts.Source
	res.Page, res.PageSize = opts.Page, opts.PageSize
	fail := func(block string, cause error) {
		res.Complete = false
		res.Partial = true
		res.Errors = append(res.Errors, block+"读取失败，相关收件箱事项可能缺失")
		common.SysWarn("收件箱聚合读取%s失败 user=%d: %v", block, userID, cause)
	}

	if rows, readErr := loadActiveRevertedCorpAdjusts(userID); readErr != nil {
		fail("已撤销公司行动", readErr)
	} else {
		res.Items = append(res.Items, rows...)
	}
	if rows, readErr := loadActiveJobFailures(userID, time.Now().AddDate(0, 0, -todoHistoryMaxDays)); readErr != nil {
		fail("用户任务失败", readErr)
	} else {
		res.Items = append(res.Items, rows...)
	}

	if readErr := enrichTodoItems(userID, res.Date, res.Items); readErr != nil {
		fail("来源版本", readErr)
	}
	completed, completedErrs := loadCompletedTodoItems(userID, time.Now().AddDate(0, 0, -opts.HistoryDays))
	for block, readErr := range completedErrs {
		fail(block, readErr)
	}
	if readErr := enrichTodoItems(userID, res.Date, completed); readErr != nil {
		fail("已完成来源版本", readErr)
	}
	for i := range completed {
		completed[i].Status = TodoStatusCompleted
		completed[i].CanComplete = false
	}
	res.Items = append(res.Items, completed...)

	states, readErr := loadTodoInboxStates(userID)
	if readErr != nil {
		fail("用户收件箱状态", readErr)
	} else {
		applyTodoInboxStates(res.Items, states)
	}

	historyCutoff := time.Now().AddDate(0, 0, -opts.HistoryDays)
	windowed := make([]TodoItem, 0, len(res.Items))
	for _, item := range res.Items {
		if item.Status == TodoStatusCompleted && item.Time != nil && item.Time.Before(historyCutoff) {
			continue
		}
		windowed = append(windowed, item)
	}
	visible := suppressSnoozedAndMuted(windowed, time.Now())
	filtered := make([]TodoItem, 0, len(visible))
	for _, item := range visible {
		if opts.Scope != TodoScopeAll && item.Scope != opts.Scope {
			continue
		}
		if opts.Source != "" && opts.Source != item.SourceKind && opts.Source != item.Kind {
			continue
		}
		if !todoStatusMatches(opts.Status, item.Status) {
			continue
		}
		filtered = append(filtered, item)
	}

	groups := groupTodoItems(filtered)
	sort.Slice(groups, func(i, j int) bool { return todoItemLess(groups[i], groups[j]) })
	res.StatusCounts = map[string]int{TodoStatusNeedsAction: 0, TodoStatusAwareness: 0, TodoStatusCompleted: 0}
	res.SourceCounts = map[string]int{}
	res.ScopeCounts = map[string]int{TodoScopeLedger: 0, TodoScopeResearch: 0, TodoScopeMarket: 0}
	for _, group := range groupTodoItems(visible) {
		res.StatusCounts[group.Status]++
		res.ScopeCounts[group.Scope]++
		seen := map[string]bool{}
		for _, child := range group.Children {
			if !seen[child.SourceKind] {
				res.SourceCounts[child.SourceKind]++
				seen[child.SourceKind] = true
			}
		}
	}

	res.MatchedTotal = len(groups)
	res.Total = len(groups)
	res.Filtered = len(groupTodoItems(visible)) - len(groups)
	res.Alerts, res.Reviews = 0, 0
	for _, group := range groups {
		if groupHasKind(group, TodoKindAlert) {
			res.Alerts++
		}
		if groupHasReview(group) {
			res.Reviews++
		}
	}

	if opts.Status == TodoStatusCompleted || opts.Status == TodoStatusAll {
		start := (opts.Page - 1) * opts.PageSize
		if start > len(groups) {
			start = len(groups)
		}
		end := start + opts.PageSize
		if end > len(groups) {
			end = len(groups)
		}
		res.HasMore = end < len(groups)
		groups = groups[start:end]
	}
	if opts.Limit > 0 && len(groups) > opts.Limit {
		res.HasMore = true
		groups = groups[:opts.Limit]
	}
	res.Items = groups
	res.Partial = !res.Complete
	return res, nil
}

func todoStatusMatches(filter, status string) bool {
	switch filter {
	case TodoStatusOpen:
		return status == TodoStatusNeedsAction || status == TodoStatusAwareness
	case TodoStatusAll:
		return true
	default:
		return filter == status
	}
}

func loadActiveRevertedCorpAdjusts(userID int64) ([]TodoItem, error) {
	rows, err := ListCorpAdjusts(userID, model.CorpAdjustReverted)
	if err != nil {
		return nil, err
	}
	out := make([]TodoItem, 0, len(rows))
	for _, row := range rows {
		t := row.UpdatedAt
		out = append(out, TodoItem{
			Kind: TodoKindCorpAdjust, Scope: TodoScopeLedger, Priority: 1,
			Symbol: row.Symbol, Market: row.Market, Name: row.Name,
			Title: "公司行动折算已撤销", Detail: "账本折算已撤销，请回到持仓页重新确认或忽略",
			RefID: row.ID, RefType: "positions", Time: &t,
		})
	}
	return out, nil
}

func loadActiveJobFailures(userID int64, cutoff time.Time) ([]TodoItem, error) {
	var rows []model.JobFailureNotification
	err := common.DB.Where("user_id = ? AND created_at >= ?", userID, cutoff).
		Order("created_at DESC, id DESC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]TodoItem, 0, len(rows))
	for _, row := range rows {
		// ErrorSummary 在正常写入路径已脱敏；投影仍只按白名单错误码重建摘要，
		// 防止旧行或人工导入行把原始错误、请求或密钥带进收件箱。
		detail := jobFailureSummary(row.ErrorCode)
		t := row.CreatedAt
		out = append(out, TodoItem{
			Kind: TodoKindJobFailure, Scope: TodoScopeResearch, Priority: 2,
			Title: "用户任务执行失败", Detail: detail,
			RefID: row.ID, RefType: "tasks", DeepLink: fmt.Sprintf("/tasks?job_id=%d", row.JobRunID), Time: &t,
		})
	}
	return out, nil
}

func loadCompletedTodoItems(userID int64, cutoff time.Time) ([]TodoItem, map[string]error) {
	out := []TodoItem{}
	errs := map[string]error{}
	var events []model.AlertEvent
	if err := common.DB.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.AlertEventRead, model.AlertEventDismissed}, cutoff).Find(&events).Error; err != nil {
		errs["提醒完成历史"] = err
	} else {
		for _, event := range events {
			t := event.UpdatedAt
			out = append(out, TodoItem{Kind: TodoKindAlert, Scope: TodoScopeResearch, Priority: 3,
				Symbol: event.Symbol, Market: event.Market, Name: event.Name, Title: "条件提醒已收下", Detail: event.Message,
				RefID: event.ID, RefType: "alerts", DeepLink: alertEventDeepLink(event.ID), Time: &t})
		}
	}
	var recs []model.RecommendationStatus
	if err := common.DB.Where("user_id = ? AND review_needed = ? AND review_ack = ? AND updated_at >= ?",
		userID, true, true, cutoff).Find(&recs).Error; err != nil {
		errs["推荐复盘完成历史"] = err
	} else {
		for _, row := range recs {
			t := row.UpdatedAt
			out = append(out, TodoItem{Kind: TodoKindRecReview, Scope: TodoScopeResearch, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Symbol, Title: "推荐复盘已收下", Detail: recReviewDetail(row),
				RefID: row.ID, RefType: "recommendations", DeepLink: "/recommendations", Time: &t})
		}
	}
	var reviews []model.SellReview
	if err := common.DB.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.SellReviewStatusResolved, model.SellReviewStatusDismissed}, cutoff).Find(&reviews).Error; err != nil {
		errs["卖出复核完成历史"] = err
	} else {
		for _, row := range reviews {
			t := row.UpdatedAt
			out = append(out, TodoItem{Kind: TodoKindSellReview, Scope: TodoScopeLedger, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Name, Title: "卖出复核已完成 · " + row.Title, Detail: row.Detail,
				RefID: row.ID, RefType: "positions", DeepLink: fmt.Sprintf("/positions?position_id=%d", row.PositionID), Time: &t})
		}
	}
	var adjusts []model.PositionCorpAdjust
	if err := common.DB.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.CorpAdjustConfirmed, model.CorpAdjustDismissed}, cutoff).Find(&adjusts).Error; err != nil {
		errs["公司行动完成历史"] = err
	} else {
		for _, row := range adjusts {
			t := row.UpdatedAt
			out = append(out, TodoItem{Kind: TodoKindCorpAdjust, Scope: TodoScopeLedger, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Name, Title: "公司行动已处理", Detail: row.PlanProfile,
				RefID: row.ID, RefType: "positions", DeepLink: fmt.Sprintf("/positions?position_id=%d", row.PositionID), Time: &t})
		}
	}
	return out, errs
}

func enrichTodoItems(userID int64, today string, items []TodoItem) error {
	var firstErr error
	for i := range items {
		if err := enrichTodoItem(userID, today, &items[i]); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && firstErr == nil {
			firstErr = err
		}
		if items[i].SourceVersion == "" {
			items[i].SourceKind = items[i].Kind
			items[i].SourceID = items[i].RefID
			items[i].SourceVersion = fmt.Sprintf("legacy:%s:%d:%s", items[i].Kind, items[i].RefID, today)
		}
	}
	return firstErr
}

func enrichTodoItem(userID int64, today string, item *TodoItem) error {
	item.Status = TodoStatusNeedsAction
	item.SourceKind, item.SourceID = item.Kind, item.RefID
	item.SourceLabel = todoSourceLabel(item.Kind)
	item.Severity, item.severityRank = "medium", 2
	item.eventDate = today
	item.groupCategory = item.Kind
	item.CanComplete = false
	item.sortBucket = 2

	switch item.Kind {
	case TodoKindAlert:
		var row model.AlertEvent
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.eventDate = row.TradeDate
		if item.eventDate == "" {
			item.eventDate = row.TriggeredAt.In(time.Local).Format("2006-01-02")
		}
		item.SourceVersion = todoVersion(row.Status, item.eventDate, row.ContextVersion, row.UpdatedAt)
		item.groupCategory = "alert:" + row.Kind
		item.CanComplete = row.Status == model.AlertEventUnread
		if isPositionAlertKind(row.Kind) {
			item.Scope, item.Status, item.sortBucket = TodoScopeLedger, TodoStatusNeedsAction, 1
			item.Severity, item.severityRank = "high", 3
		} else {
			item.Status, item.sortBucket = TodoStatusAwareness, 3
		}
		if row.Status != model.AlertEventUnread {
			item.Status = TodoStatusCompleted
		}
	case TodoKindRecReview:
		var row model.RecommendationStatus
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Outcome, fmt.Sprint(row.ReviewAck), row.LastEvalDate, row.UpdatedAt)
		item.CanComplete = row.ReviewNeeded && !row.ReviewAck
		item.DeepLink = "/recommendations"
		if row.Outcome == model.RecOutcomeStopLoss {
			item.Severity, item.severityRank = "high", 3
		}
		if row.ReviewAck {
			item.Status = TodoStatusCompleted
		}
	case TodoKindStopLoss, TodoKindPositionShort, TodoKindPositionLong:
		var row model.Position
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		state := item.Kind
		if item.Kind == TodoKindStopLoss {
			item.sortBucket, item.groupCategory = 0, "position:stop_loss"
			if strings.Contains(item.Title, "跌破") {
				item.Severity, item.severityRank, state = "critical", 4, "below"
			} else {
				item.Severity, item.severityRank, state = "high", 3, "near"
			}
		} else {
			item.Severity, item.severityRank = "low", 1
			item.groupCategory = "position:review"
		}
		item.SourceVersion = todoVersion(today, state, row.UpdatedAt)
		item.DeepLink = fmt.Sprintf("/positions?position_id=%d", row.ID)
	case TodoKindThesisDue:
		var row model.ThesisCard
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Status, row.NextReviewDate, row.UpdatedAt)
		item.Severity, item.severityRank, item.sortBucket = "low", 1, 1
		item.DueAt = parseLocalDate(row.NextReviewDate)
		item.DeepLink = fmt.Sprintf("/thesis?card_id=%d", row.ID)
	case TodoKindCorpAdjust:
		var row model.PositionCorpAdjust
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Status, row.ExDate, row.UpdatedAt)
		item.Severity, item.severityRank, item.sortBucket = "critical", 4, 0
		item.groupCategory = "ledger:corp_adjust"
		item.DueAt = parseLocalDate(row.ExDate)
		item.DeepLink = fmt.Sprintf("/positions?position_id=%d", row.PositionID)
		if row.Status == model.CorpAdjustConfirmed || row.Status == model.CorpAdjustDismissed {
			item.Status = TodoStatusCompleted
		}
	case TodoKindIpo:
		var row model.IpoSubscription
		if err := common.DB.Where("id = ?", item.RefID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Kind, row.ApplyDate, row.ApplyCode, row.UpdatedAt)
		item.Status, item.Severity, item.severityRank, item.sortBucket = TodoStatusAwareness, "info", 0, 3
		item.eventDate = row.ApplyDate
		item.DueAt = parseLocalDate(row.ApplyDate)
		item.CanComplete = true
		item.DeepLink = "/today?source=ipo"
	case TodoKindSellReview:
		var row model.SellReview
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Status, row.Severity, row.TradeDate, row.UpdatedAt)
		item.eventDate, item.groupCategory = row.TradeDate, "sell:"+row.Trigger
		item.DeepLink = fmt.Sprintf("/positions?position_id=%d", row.PositionID)
		item.CanComplete = row.Status == model.SellReviewStatusOpen
		item.Severity, item.severityRank = row.Severity, severityRank(row.Severity)
		if row.Severity == model.SellReviewSeverityHigh {
			item.sortBucket = 0
		}
		if row.Status != model.SellReviewStatusOpen {
			item.Status = TodoStatusCompleted
		}
	case TodoKindJobFailure:
		var row model.JobFailureNotification
		if err := common.DB.Where("id = ? AND user_id = ?", item.RefID, userID).First(&row).Error; err != nil {
			return err
		}
		item.SourceVersion = todoVersion(row.Status, row.MergeCount, row.ErrorCode, row.UpdatedAt)
		item.groupCategory = "job_failure:" + row.Kind
		item.eventDate = row.CreatedAt.In(time.Local).Format("2006-01-02")
		item.Severity, item.severityRank = "high", 3
		item.CanComplete = true
	}
	return nil
}

func todoVersion(parts ...any) string {
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		switch value := part.(type) {
		case time.Time:
			values = append(values, fmt.Sprint(value.UTC().UnixNano()))
		case *time.Time:
			if value == nil {
				values = append(values, "nil")
			} else {
				values = append(values, fmt.Sprint(value.UTC().UnixNano()))
			}
		default:
			values = append(values, fmt.Sprint(value))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(values, ":")))
	return fmt.Sprintf("%x", sum[:])
}

func parseLocalDate(value string) *time.Time {
	if value == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return nil
	}
	return &t
}

func severityRank(value string) int {
	switch strings.ToLower(value) {
	case "critical":
		return 4
	case "high":
		return 3
	case "med", "medium", "warning":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func todoSourceLabel(kind string) string {
	switch kind {
	case TodoKindAlert:
		return "提醒命中"
	case TodoKindRecReview:
		return "推荐复盘"
	case TodoKindStopLoss, TodoKindPositionShort, TodoKindPositionLong:
		return "持仓状态"
	case TodoKindThesisDue:
		return "逻辑卡"
	case TodoKindCorpAdjust:
		return "公司行动"
	case TodoKindIpo:
		return "打新日历"
	case TodoKindSellReview:
		return "卖出复核"
	case TodoKindJobFailure:
		return "任务中心"
	default:
		return kind
	}
}

func loadTodoInboxStates(userID int64) (map[string]model.TodoInboxState, error) {
	var rows []model.TodoInboxState
	if err := common.DB.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]model.TodoInboxState, len(rows))
	for _, row := range rows {
		out[todoStateKey(row.SourceKind, row.SourceID)] = row
	}
	return out, nil
}

func todoStateKey(kind string, id int64) string {
	return fmt.Sprintf("%s:%d", kind, id)
}

func applyTodoInboxStates(items []TodoItem, states map[string]model.TodoInboxState) {
	for i := range items {
		state, ok := states[todoStateKey(items[i].SourceKind, items[i].SourceID)]
		if !ok {
			continue
		}
		if state.SourceVersion != items[i].SourceVersion {
			items[i].versionChanged = true
			continue
		}
		items[i].Read = state.Read
		items[i].SnoozedUntil = state.SnoozedUntil
		items[i].mutedToday = state.MutedDate == time.Now().In(time.Local).Format("2006-01-02")
		if state.Read && (items[i].Kind == TodoKindIpo || items[i].Kind == TodoKindJobFailure) {
			items[i].Status = TodoStatusCompleted
			items[i].CanComplete = false
		}
	}
}

func baseTodoGroupKey(item TodoItem) string {
	date := item.eventDate
	if date == "" && item.Time != nil {
		date = item.Time.In(time.Local).Format("2006-01-02")
	}
	return strings.Join([]string{date, strings.ToLower(item.Market), strings.ToUpper(item.Symbol), item.groupCategory}, "|")
}

func suppressSnoozedAndMuted(items []TodoItem, now time.Time) []TodoItem {
	mutedRanks := map[string]int{}
	for _, item := range items {
		if item.Status != TodoStatusCompleted && item.mutedToday {
			key := baseTodoGroupKey(item)
			if item.severityRank > mutedRanks[key] {
				mutedRanks[key] = item.severityRank
			}
		}
	}
	out := make([]TodoItem, 0, len(items))
	for _, item := range items {
		if item.Status != TodoStatusCompleted && item.SnoozedUntil != nil && item.SnoozedUntil.After(now) {
			continue
		}
		if mutedRank, ok := mutedRanks[baseTodoGroupKey(item)]; ok && item.Status != TodoStatusCompleted &&
			!item.versionChanged && item.severityRank <= mutedRank {
			continue
		}
		out = append(out, item)
	}
	return out
}

func groupTodoItems(items []TodoItem) []TodoItem {
	groups := map[string][]TodoItem{}
	order := []string{}
	for _, item := range items {
		key := item.Status + "|" + baseTodoGroupKey(item)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item)
	}
	out := make([]TodoItem, 0, len(groups))
	for _, key := range order {
		children := groups[key]
		sort.Slice(children, func(i, j int) bool { return todoItemLess(children[i], children[j]) })
		parent := children[0]
		parent.GroupKey = key
		parent.ChildCount = len(children)
		parent.Children = make([]TodoChild, 0, len(children))
		parent.CanComplete = true
		maxSeverity := parent.severityRank
		for _, child := range children {
			parent.Children = append(parent.Children, todoChildFromItem(child))
			parent.CanComplete = parent.CanComplete && child.CanComplete
			if child.severityRank > maxSeverity {
				maxSeverity, parent.Severity, parent.severityRank = child.severityRank, child.Severity, child.severityRank
			}
			if child.sortBucket < parent.sortBucket {
				parent.sortBucket = child.sortBucket
			}
			if parent.DueAt == nil || (child.DueAt != nil && child.DueAt.Before(*parent.DueAt)) {
				parent.DueAt = child.DueAt
			}
		}
		if len(children) > 1 {
			parent.Title = fmt.Sprintf("%s（%d 条）", parent.Title, len(children))
			parent.SourceLabel = "多个来源"
		}
		out = append(out, parent)
	}
	return out
}

func todoChildFromItem(item TodoItem) TodoChild {
	return TodoChild{Kind: item.Kind, SourceKind: item.SourceKind, SourceID: item.SourceID,
		SourceVersion: item.SourceVersion, SourceLabel: item.SourceLabel, Title: item.Title, Detail: item.Detail,
		Severity: item.Severity, RefID: item.RefID, RefType: item.RefType, DeepLink: item.DeepLink,
		Time: item.Time, DueAt: item.DueAt, Read: item.Read, SnoozedUntil: item.SnoozedUntil,
		CanComplete: item.CanComplete}
}

func todoItemLess(a, b TodoItem) bool {
	if a.sortBucket != b.sortBucket {
		return a.sortBucket < b.sortBucket
	}
	if (a.DueAt != nil) != (b.DueAt != nil) {
		return a.DueAt != nil
	}
	if a.DueAt != nil && b.DueAt != nil && !a.DueAt.Equal(*b.DueAt) {
		return a.DueAt.Before(*b.DueAt)
	}
	if a.severityRank != b.severityRank {
		return a.severityRank > b.severityRank
	}
	scopeRank := map[string]int{TodoScopeLedger: 0, TodoScopeResearch: 1, TodoScopeMarket: 2}
	if scopeRank[a.Scope] != scopeRank[b.Scope] {
		return scopeRank[a.Scope] < scopeRank[b.Scope]
	}
	if (a.Time != nil) != (b.Time != nil) {
		return a.Time != nil
	}
	if a.Time != nil && b.Time != nil && !a.Time.Equal(*b.Time) {
		return a.Time.After(*b.Time)
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	if a.SourceKind != b.SourceKind {
		return a.SourceKind < b.SourceKind
	}
	return a.SourceID < b.SourceID
}

func groupHasKind(item TodoItem, kind string) bool {
	for _, child := range item.Children {
		if child.Kind == kind {
			return true
		}
	}
	return false
}

func groupHasReview(item TodoItem) bool {
	for _, child := range item.Children {
		switch child.Kind {
		case TodoKindRecReview, TodoKindStopLoss, TodoKindPositionShort, TodoKindPositionLong,
			TodoKindThesisDue, TodoKindCorpAdjust, TodoKindSellReview:
			return true
		}
	}
	return false
}

// ApplyInboxAction 对来源做版本校验后执行。可完成的来源调用原业务状态机；
// 持仓风险、逻辑卡和公司行动只允许稍后/当日静默，不能伪造“已完成”。
func (s *TodoService) ApplyInboxAction(userID int64, req TodoActionRequest) error {
	if len(req.Items) == 0 || len(req.Items) > 100 {
		return errors.New("请选择 1 到 100 条事项")
	}
	switch req.Action {
	case TodoActionRead, TodoActionSnooze, TodoActionMuteToday:
	default:
		return errors.New("非法的收件箱操作")
	}
	currentItems, err := s.activeTodoItems(userID)
	if err != nil {
		return err
	}
	currentRefs := make(map[string]TodoSourceRef, len(currentItems))
	currentByKey := make(map[string]TodoItem, len(currentItems))
	for _, item := range currentItems {
		key := todoStateKey(item.SourceKind, item.SourceID)
		currentRefs[key] = TodoSourceRef{SourceKind: item.SourceKind, SourceID: item.SourceID, SourceVersion: item.SourceVersion}
		currentByKey[key] = item
	}
	seen := map[string]bool{}
	for _, ref := range req.Items {
		key := todoStateKey(ref.SourceKind, ref.SourceID)
		if ref.SourceID <= 0 || ref.SourceVersion == "" || seen[key] {
			return errors.New("来源引用无效或重复")
		}
		seen[key] = true
		current, ok := currentRefs[key]
		if !ok {
			return errors.New("事项不存在、已完成或不属于当前用户")
		}
		if current.SourceVersion != ref.SourceVersion {
			return errors.New("事项已有新版本，请刷新后再操作")
		}
		if req.Action == TodoActionRead && !todoSourceCanComplete(ref.SourceKind) {
			return errors.New("该事项不能直接完成，请稍后处理或前往原页面")
		}
	}
	if req.Action == TodoActionMuteToday {
		mutedGroups := map[string]bool{}
		for _, ref := range req.Items {
			mutedGroups[baseTodoGroupKey(currentByKey[todoStateKey(ref.SourceKind, ref.SourceID)])] = true
		}
		if len(req.Items) > 100 {
			return errors.New("同组事项超过 100 条，请先按来源处理")
		}
		for _, item := range currentItems {
			key := todoStateKey(item.SourceKind, item.SourceID)
			if !mutedGroups[baseTodoGroupKey(item)] || seen[key] {
				continue
			}
			req.Items = append(req.Items, TodoSourceRef{
				SourceKind: item.SourceKind, SourceID: item.SourceID, SourceVersion: item.SourceVersion,
			})
			seen[key] = true
		}
	}
	for _, ref := range req.Items {
		if err := s.applyOneInboxAction(userID, req.Action, ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *TodoService) activeTodoItems(userID int64) ([]TodoItem, error) {
	res, err := s.buildActive(context.Background(), userID, TodoScopeAll)
	if err != nil {
		return nil, err
	}
	rows, err := loadActiveRevertedCorpAdjusts(userID)
	if err == nil {
		res.Items = append(res.Items, rows...)
	}
	jobs, err := loadActiveJobFailures(userID, time.Now().AddDate(0, 0, -todoHistoryMaxDays))
	if err == nil {
		res.Items = append(res.Items, jobs...)
	}
	if err := enrichTodoItems(userID, res.Date, res.Items); err != nil {
		return nil, err
	}
	return res.Items, nil
}

func todoSourceCanComplete(kind string) bool {
	switch kind {
	case TodoKindAlert, TodoKindRecReview, TodoKindSellReview, TodoKindIpo, TodoKindJobFailure:
		return true
	default:
		return false
	}
}

func (s *TodoService) applyOneInboxAction(userID int64, action string, ref TodoSourceRef) error {
	state := model.TodoInboxState{UserID: userID, SourceKind: ref.SourceKind, SourceID: ref.SourceID, SourceVersion: ref.SourceVersion}
	now := time.Now()
	switch action {
	case TodoActionSnooze:
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 9, 0, 0, 0, time.Local)
		state.SnoozedUntil = &tomorrow
	case TodoActionMuteToday:
		state.MutedDate = now.In(time.Local).Format("2006-01-02")
	case TodoActionRead:
		switch ref.SourceKind {
		case TodoKindAlert:
			if _, err := s.alert.SetEventStatus(userID, ref.SourceID, model.AlertEventRead); err != nil {
				return err
			}
		case TodoKindRecReview:
			if err := NewTrackingService(nil).AckReview(userID, ref.SourceID); err != nil {
				return err
			}
		case TodoKindSellReview:
			if _, err := SetSellReviewStatus(userID, ref.SourceID, model.SellReviewStatusResolved); err != nil {
				return err
			}
		case TodoKindIpo, TodoKindJobFailure:
		default:
			return errors.New("该事项不能直接完成")
		}
		current, err := currentTodoSourceRef(userID, ref.SourceKind, ref.SourceID)
		if err != nil {
			return err
		}
		state.SourceVersion = current.SourceVersion
		state.Read = true
	}
	return upsertTodoInboxState(state)
}

func upsertTodoInboxState(state model.TodoInboxState) error {
	return common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "source_kind"}, {Name: "source_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"source_version": state.SourceVersion, "read": state.Read,
			"snoozed_until": state.SnoozedUntil, "muted_date": state.MutedDate,
			"updated_at": time.Now(),
		}),
	}).Create(&state).Error
}

func currentTodoSourceRef(userID int64, kind string, id int64) (TodoSourceRef, error) {
	item := TodoItem{Kind: kind, RefID: id}
	if err := enrichTodoItem(userID, time.Now().In(time.Local).Format("2006-01-02"), &item); err != nil {
		return TodoSourceRef{}, err
	}
	return TodoSourceRef{SourceKind: item.SourceKind, SourceID: item.SourceID, SourceVersion: item.SourceVersion}, nil
}
