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
	ctx = jobSubmissionContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	if rows, readErr := loadActiveJobFailures(userID, time.Now().AddDate(0, 0, -todoHistoryMaxDays), res.Date, ctx); readErr != nil {
		fail("用户任务失败", readErr)
	} else {
		res.Items = append(res.Items, rows...)
	}

	completed, completedErrs := loadCompletedTodoItems(userID, time.Now().AddDate(0, 0, -opts.HistoryDays), res.Date, ctx)
	for block, readErr := range completedErrs {
		fail(block, readErr)
	}
	for i := range completed {
		completed[i].Status = TodoStatusCompleted
		completed[i].CanComplete = false
	}
	// 两次列表读取之间完成的同一来源只保留后读到的完成事实。
	completedKeys := make(map[string]bool, len(completed))
	for _, item := range completed {
		completedKeys[todoStateKey(item.SourceKind, item.SourceID)] = true
	}
	active := res.Items[:0]
	for _, item := range res.Items {
		if !completedKeys[todoStateKey(item.SourceKind, item.SourceID)] {
			active = append(active, item)
		}
	}
	res.Items = append(active, completed...)

	states, readErr := loadTodoInboxStates(ctx, userID)
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
		start := len(groups)
		if opts.Page-1 <= len(groups)/opts.PageSize {
			start = (opts.Page - 1) * opts.PageSize
		}
		end := start + min(opts.PageSize, len(groups)-start)
		res.HasMore = end < len(groups)
		groups = groups[start:end]
	}
	if opts.Limit > 0 && len(groups) > opts.Limit {
		res.HasMore = true
		groups = groups[:opts.Limit]
	}
	res.Items = groups
	res.Partial = !res.Complete
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

func loadActiveJobFailures(userID int64, cutoff time.Time, today string, contexts ...context.Context) ([]TodoItem, error) {
	var rows []model.JobFailureNotification
	err := common.DB.WithContext(jobSubmissionContext(contexts...)).Where("user_id = ? AND created_at >= ?", userID, cutoff).
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
		out = append(out, todoItemFromSource(today, TodoItem{
			Kind: TodoKindJobFailure, Scope: TodoScopeResearch, Priority: 2,
			Title: "用户任务执行失败", Detail: detail,
			RefID: row.ID, RefType: "tasks", DeepLink: fmt.Sprintf("/tasks?job_id=%d", row.JobRunID), Time: &t,
		}, &row))
	}
	return out, nil
}

func loadCompletedTodoItems(userID int64, cutoff time.Time, today string, contexts ...context.Context) ([]TodoItem, map[string]error) {
	out := []TodoItem{}
	errs := map[string]error{}
	ctx := jobSubmissionContext(contexts...)
	db := common.DB.WithContext(ctx)
	heldSymbols, _, err := heldPositionStateFor(userID, ctx)
	if err != nil {
		errs["完成历史持仓归属"] = err
	}
	assessedPositions := map[int64]bool{}
	var assessedIDs []int64
	if err := db.Model(&model.PositionExitAssessment{}).Where("user_id = ?", userID).
		Distinct().Pluck("position_id", &assessedIDs).Error; err != nil {
		errs["持仓卖出风险完成历史"] = err
	} else {
		for _, id := range assessedIDs {
			assessedPositions[id] = true
		}
	}
	var events []model.AlertEvent
	if err := db.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.AlertEventRead, model.AlertEventDismissed}, cutoff).Find(&events).Error; err != nil {
		errs["提醒完成历史"] = err
	} else {
		for _, event := range events {
			if isPositionAlertKind(event.Kind) && assessedPositions[event.PositionID] {
				continue // 统一评估上线后，持仓类底层事件只留审计，不重复进入收件箱历史
			}
			t := event.UpdatedAt
			scope := TodoScopeResearch
			if isPositionAlertKind(event.Kind) || heldSymbols[QuoteKey(event.Market, event.Symbol)] {
				scope = TodoScopeLedger
			}
			out = append(out, todoItemFromSource(today, TodoItem{Kind: TodoKindAlert, Scope: scope, Priority: 3,
				Symbol: event.Symbol, Market: event.Market, Name: event.Name, Title: "条件提醒已收下", Detail: event.Message,
				RefID: event.ID, RefType: "alerts", DeepLink: alertEventDeepLink(event.ID), Time: &t}, &event))
		}
	}
	var reviews []model.SellReview
	if err := db.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.SellReviewStatusResolved, model.SellReviewStatusDismissed}, cutoff).Find(&reviews).Error; err != nil {
		errs["卖出复核完成历史"] = err
	} else {
		for _, row := range reviews {
			if assessedPositions[row.PositionID] {
				continue
			}
			t := row.UpdatedAt
			out = append(out, todoItemFromSource(today, TodoItem{Kind: TodoKindSellReview, Scope: TodoScopeLedger, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Name, Title: "卖出复核已完成 · " + row.Title, Detail: row.Detail,
				RefID: row.ID, RefType: "positions", DeepLink: fmt.Sprintf("/positions?position_id=%d", row.PositionID), Time: &t}, &row))
		}
	}
	var recs []model.RecommendationStatus
	if err := db.Where("user_id = ? AND review_needed = ? AND review_ack = ? AND updated_at >= ?",
		userID, true, true, cutoff).Find(&recs).Error; err != nil {
		errs["推荐复盘完成历史"] = err
	} else {
		for _, row := range recs {
			t := row.UpdatedAt
			out = append(out, todoItemFromSource(today, TodoItem{Kind: TodoKindRecReview, Scope: TodoScopeResearch, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Symbol, Title: "推荐复盘已收下", Detail: recReviewDetail(row),
				RefID: row.ID, RefType: "recommendations", DeepLink: "/recommendations", Time: &t}, &row))
		}
	}
	var adjusts []model.PositionCorpAdjust
	if err := db.Where("user_id = ? AND status IN ? AND updated_at >= ?", userID,
		[]string{model.CorpAdjustConfirmed, model.CorpAdjustDismissed}, cutoff).Find(&adjusts).Error; err != nil {
		errs["公司行动完成历史"] = err
	} else {
		for _, row := range adjusts {
			t := row.UpdatedAt
			out = append(out, todoItemFromSource(today, TodoItem{Kind: TodoKindCorpAdjust, Scope: TodoScopeLedger, Priority: 3,
				Symbol: row.Symbol, Market: row.Market, Name: row.Name, Title: "公司行动已处理", Detail: row.PlanProfile,
				RefID: row.ID, RefType: "positions", DeepLink: fmt.Sprintf("/positions?position_id=%d", row.PositionID), Time: &t}, &row))
		}
	}
	return out, errs
}

func enrichTodoItemDB(db *gorm.DB, userID int64, today string, item *TodoItem) error {
	var source any
	switch item.Kind {
	case TodoKindAlert:
		source = &model.AlertEvent{}
	case TodoKindRecReview:
		source = &model.RecommendationStatus{}
	case TodoKindStopLoss, TodoKindPositionShort, TodoKindPositionLong:
		source = &model.Position{}
	case TodoKindThesisDue:
		source = &model.ThesisCard{}
	case TodoKindCorpAdjust:
		source = &model.PositionCorpAdjust{}
	case TodoKindIpo:
		source = &model.IpoSubscription{}
	case TodoKindSellReview:
		source = &model.SellReview{}
	case TodoKindPositionExit:
		source = &model.PositionExitAssessment{}
	case TodoKindJobFailure:
		source = &model.JobFailureNotification{}
	default:
		return errors.New("未知的收件箱来源")
	}
	query := db.Where("id = ?", item.RefID)
	if item.Kind != TodoKindIpo {
		query = query.Where("user_id = ?", userID)
	}
	if err := query.First(source).Error; err != nil {
		return err
	}
	*item = todoItemFromSource(today, *item, source)
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
	case TodoKindPositionExit:
		return "持仓卖出风险"
	case TodoKindJobFailure:
		return "任务中心"
	default:
		return kind
	}
}

func loadTodoInboxStates(ctx context.Context, userID int64) (map[string]model.TodoInboxState, error) {
	var rows []model.TodoInboxState
	if err := common.DB.WithContext(ctx).Where("user_id = ?", userID).Find(&rows).Error; err != nil {
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
		if state.Read && (items[i].Kind == TodoKindIpo || items[i].Kind == TodoKindJobFailure || items[i].Kind == TodoKindPositionExit) {
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
			if rank, exists := mutedRanks[key]; !exists || item.severityRank > rank {
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
			TodoKindThesisDue, TodoKindCorpAdjust, TodoKindSellReview, TodoKindPositionExit:
			return true
		}
	}
	return false
}

// ApplyInboxAction 对来源做版本校验后执行。可完成的来源调用原业务状态机；
// 持仓风险、逻辑卡和公司行动只允许稍后/当日静默，不能伪造“已完成”。
func (s *TodoService) ApplyInboxAction(userID int64, req TodoActionRequest) error {
	return s.ApplyInboxActionContext(context.Background(), userID, req)
}

func (s *TodoService) ApplyInboxActionContext(ctx context.Context, userID int64, req TodoActionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(req.Items) == 0 || len(req.Items) > 100 {
		return errors.New("请选择 1 到 100 条事项")
	}
	switch req.Action {
	case TodoActionRead, TodoActionSnooze, TodoActionMuteToday:
	default:
		return errors.New("非法的收件箱操作")
	}
	currentItems, err := s.activeTodoItems(ctx, userID)
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
		for _, item := range currentItems {
			key := todoStateKey(item.SourceKind, item.SourceID)
			if !mutedGroups[baseTodoGroupKey(item)] || seen[key] {
				continue
			}
			if len(req.Items) >= 100 {
				return errors.New("同组事项超过 100 条，请先按来源处理")
			}
			req.Items = append(req.Items, TodoSourceRef{
				SourceKind: item.SourceKind, SourceID: item.SourceID, SourceVersion: item.SourceVersion,
			})
			seen[key] = true
		}
	}
	// 固定锁顺序；版本校验、原业务状态和收件箱状态在同一事务内提交。
	refs := append([]TodoSourceRef(nil), req.Items...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].SourceKind != refs[j].SourceKind {
			return refs[i].SourceKind < refs[j].SourceKind
		}
		return refs[i].SourceID < refs[j].SourceID
	})
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, ref := range refs {
			item := currentByKey[todoStateKey(ref.SourceKind, ref.SourceID)]
			locked := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Session(&gorm.Session{})
			if ref.SourceKind == TodoKindPositionExit {
				var latest model.PositionExitAssessment
				if err := locked.Where("user_id = ? AND position_id = ?", userID, ref.SourceID).
					Order("evaluated_at DESC, id DESC").First(&latest).Error; err != nil {
					return err
				}
				item.RefID = latest.ID
			}
			if err := enrichTodoItemDB(locked, userID, time.Now().In(time.Local).Format("2006-01-02"), &item); err != nil {
				return err
			}
			if item.SourceVersion != ref.SourceVersion {
				return errors.New("事项已有新版本，请刷新后再操作")
			}
			if req.Action == TodoActionRead && !item.CanComplete {
				return errors.New("事项已完成或不能直接完成，请刷新后再操作")
			}
			var state model.TodoInboxState
			stateErr := locked.Where("user_id = ? AND source_kind = ? AND source_id = ?", userID, ref.SourceKind, ref.SourceID).
				First(&state).Error
			if stateErr != nil && !errors.Is(stateErr, gorm.ErrRecordNotFound) {
				return stateErr
			}
			if stateErr == nil && state.SourceVersion == ref.SourceVersion && state.Read {
				return errors.New("事项已完成，请刷新后再操作")
			}
			if err := s.applyOneInboxActionDB(tx, userID, req.Action, ref); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *TodoService) activeTodoItems(ctx context.Context, userID int64) ([]TodoItem, error) {
	res, err := s.buildActive(ctx, userID, TodoScopeAll)
	if err != nil {
		return nil, err
	}
	if !res.Complete {
		return nil, errors.New("收件箱来源读取不完整，请刷新后重试")
	}
	jobs, err := loadActiveJobFailures(userID, time.Now().AddDate(0, 0, -todoHistoryMaxDays), res.Date, ctx)
	if err != nil {
		return nil, err
	}
	res.Items = append(res.Items, jobs...)
	states, err := loadTodoInboxStates(ctx, userID)
	if err != nil {
		return nil, err
	}
	applyTodoInboxStates(res.Items, states)
	active := res.Items[:0]
	for _, item := range res.Items {
		if item.Status != TodoStatusCompleted {
			active = append(active, item)
		}
	}
	return active, nil
}

func todoSourceCanComplete(kind string) bool {
	switch kind {
	case TodoKindAlert, TodoKindRecReview, TodoKindSellReview, TodoKindPositionExit, TodoKindIpo, TodoKindJobFailure:
		return true
	default:
		return false
	}
}

func (s *TodoService) applyOneInboxActionDB(db *gorm.DB, userID int64, action string, ref TodoSourceRef) error {
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
			if _, err := s.alert.setEventStatusDB(db, userID, ref.SourceID, model.AlertEventRead); err != nil {
				return err
			}
		case TodoKindRecReview:
			if err := ackReviewDB(db, userID, ref.SourceID); err != nil {
				return err
			}
		case TodoKindSellReview:
			if _, err := setSellReviewStatusDB(db, userID, ref.SourceID, model.SellReviewStatusResolved); err != nil {
				return err
			}
		case TodoKindPositionExit, TodoKindIpo, TodoKindJobFailure:
		default:
			return errors.New("该事项不能直接完成")
		}
		// 只为本事务实际改变了原状态的来源更新版本。纯收件箱确认必须保留用户
		// 看过的版本，不能重读后顺带收下刚出现的新风险/新失败记录。
		if ref.SourceKind == TodoKindAlert || ref.SourceKind == TodoKindRecReview || ref.SourceKind == TodoKindSellReview {
			current, err := currentTodoSourceRefDB(db.Clauses(clause.Locking{Strength: "UPDATE"}), userID, ref.SourceKind, ref.SourceID)
			if err != nil {
				return err
			}
			state.SourceVersion = current.SourceVersion
		}
		state.Read = true
	}
	return upsertTodoInboxStateDB(db, state)
}

func upsertTodoInboxState(state model.TodoInboxState) error {
	return upsertTodoInboxStateDB(common.DB, state)
}

func upsertTodoInboxStateDB(db *gorm.DB, state model.TodoInboxState) error {
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "source_kind"}, {Name: "source_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"source_version": state.SourceVersion, "read": state.Read,
			"snoozed_until": state.SnoozedUntil, "muted_date": state.MutedDate,
			"updated_at": time.Now(),
		}),
	}).Create(&state).Error
}

func currentTodoSourceRef(userID int64, kind string, id int64) (TodoSourceRef, error) {
	return currentTodoSourceRefDB(common.DB, userID, kind, id)
}

func currentTodoSourceRefDB(db *gorm.DB, userID int64, kind string, id int64) (TodoSourceRef, error) {
	if kind == TodoKindPositionExit {
		var latest model.PositionExitAssessment
		if err := db.Where("user_id = ? AND position_id = ?", userID, id).
			Order("evaluated_at DESC, id DESC").First(&latest).Error; err != nil {
			return TodoSourceRef{}, err
		}
		id = latest.ID
	}
	item := TodoItem{Kind: kind, RefID: id}
	if err := enrichTodoItemDB(db, userID, time.Now().In(time.Local).Format("2006-01-02"), &item); err != nil {
		return TodoSourceRef{}, err
	}
	return TodoSourceRef{SourceKind: item.SourceKind, SourceID: item.SourceID, SourceVersion: item.SourceVersion}, nil
}
