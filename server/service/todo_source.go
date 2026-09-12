package service

import (
	"fmt"
	"strings"
	"time"

	"quantvista/model"
)

// 正文、状态和操作版本必须来自同一次读取；不能读取完正文后再取最新版本。
// 写操作另外通过锁定读重新构造元数据，用于核验用户实际看过的版本。
func todoItemFromSource(today string, item TodoItem, source any) TodoItem {
	item.Status = TodoStatusNeedsAction
	item.SourceKind, item.SourceID = item.Kind, item.RefID
	item.SourceLabel = todoSourceLabel(item.Kind)
	item.Severity, item.severityRank = "medium", 2
	item.eventDate = today
	item.groupCategory = item.Kind
	item.CanComplete = false
	item.sortBucket = 2

	switch row := source.(type) {
	case *model.AlertEvent:
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
	case *model.RecommendationStatus:
		item.SourceVersion = todoVersion(row.Outcome, fmt.Sprint(row.ReviewAck), row.LastEvalDate, row.UpdatedAt)
		item.CanComplete = row.ReviewNeeded && !row.ReviewAck
		item.DeepLink = "/recommendations"
		if row.Outcome == model.RecOutcomeStopLoss {
			item.Severity, item.severityRank = "high", 3
		}
		if row.ReviewAck {
			item.Status = TodoStatusCompleted
		}
	case *model.Position:
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
		item.DeepLink = todoPositionDeepLink(row.ID, row.AccountID)
	case *model.ThesisCard:
		item.SourceVersion = todoVersion(row.Status, row.NextReviewDate, row.UpdatedAt)
		item.Severity, item.severityRank, item.sortBucket = "low", 1, 1
		item.DueAt = parseLocalDate(row.NextReviewDate)
		item.DeepLink = fmt.Sprintf("/thesis?card_id=%d", row.ID)
	case *model.PositionCorpAdjust:
		item.SourceVersion = todoVersion(row.Status, row.ExDate, row.UpdatedAt)
		item.Severity, item.severityRank, item.sortBucket = "critical", 4, 0
		item.groupCategory = "ledger:corp_adjust"
		item.DueAt = parseLocalDate(row.ExDate)
		item.DeepLink = todoPositionDeepLink(row.PositionID, row.AccountID)
		if row.Status == model.CorpAdjustConfirmed || row.Status == model.CorpAdjustDismissed {
			item.Status = TodoStatusCompleted
		}
	case *model.IpoSubscription:
		item.SourceVersion = todoVersion(row.Kind, row.ApplyDate, row.ApplyCode, row.UpdatedAt)
		item.Status, item.Severity, item.severityRank, item.sortBucket = TodoStatusAwareness, "info", 0, 3
		item.eventDate = row.ApplyDate
		item.DueAt = parseLocalDate(row.ApplyDate)
		item.CanComplete = true
		item.DeepLink = "/today?source=ipo&status=awareness"
	case *model.SellReview:
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
	case *model.PositionExitAssessment:
		// 同一持仓是稳定来源，评估事实哈希/交易日/等级才是版本。盘后为同一事实
		// 追加 close 快照时 assessment_id 会变化，但用户已处理状态不能因此失效。
		item.SourceID = row.PositionID
		item.SourceVersion = todoVersion(row.FactHash, row.Level, row.TradeDate, row.Version)
		item.eventDate = row.TradeDate
		if row.ActionKey != "" {
			item.SourceVersion = todoVersion(row.ActionKey, row.Level, row.Version)
			item.eventDate = row.ActionDate
		}
		item.groupCategory = fmt.Sprintf("position_exit:%d", row.PositionID)
		item.DeepLink = todoPositionDeepLink(row.PositionID, 0)
		item.CanComplete = row.ShouldTodo
		if row.Level == model.PositionExitLevelUrgent {
			item.Severity, item.severityRank, item.sortBucket = "critical", 4, 0
		} else {
			item.Severity, item.severityRank, item.sortBucket = "high", 3, 1
		}
	case *model.JobFailureNotification:
		item.SourceVersion = todoVersion(row.Status, row.MergeCount, row.ErrorCode, row.UpdatedAt)
		item.groupCategory = "job_failure:" + row.Kind
		item.eventDate = row.CreatedAt.In(time.Local).Format("2006-01-02")
		item.Severity, item.severityRank = "high", 3
		item.CanComplete = true
	}
	return item
}

func todoPositionDeepLink(positionID, accountID int64) string {
	link := fmt.Sprintf("/positions?position_id=%d", positionID)
	if accountID > 0 {
		link += fmt.Sprintf("&account_id=%d", accountID)
	}
	return link
}
