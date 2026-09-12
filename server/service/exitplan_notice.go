package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"
	"quantvista/common"
	"quantvista/model"
)

func positionExitActionSignature(row model.PositionExitAssessment) string {
	view := decodePositionExitAssessment(row)
	keys := []string{row.Level, row.PrimarySignal, row.ParamsHash}
	for _, signal := range view.Signals {
		key := signal.Key + ":" + signal.Severity
		switch signal.Key {
		case "plan_stop", "plan_take", model.AlertKindCostGain, model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown:
			key += fmt.Sprintf(":%.4f", signal.Threshold)
		}
		keys = append(keys, key)
	}
	if view.ExitPlan != nil {
		keys = append(keys, view.ExitPlan.Initial.Hash)
	}
	sort.Strings(keys)
	return stablePositionExitHash(keys)
}

func assignPositionExitAction(row *model.PositionExitAssessment, previous model.PositionExitAssessment) {
	if !row.ShouldTodo {
		row.ActionKey, row.ActionDate = "", ""
		return
	}
	if plan := decodeExitPlan(row.PlanJSON, row.PlanHash); plan != nil {
		if key, date := exitPlanActionIdentity(*plan, row.PrimarySignal); key != "" {
			row.ActionKey = stablePositionExitHash(struct {
				UserID, PositionID  int64
				Version, Level, Key string
			}{row.UserID, row.PositionID, row.Version, row.Level, key})
			row.ActionDate = date
			return
		}
	}
	signature := positionExitActionSignature(*row)
	if previous.ShouldTodo && previous.ActionKey != "" && signature == positionExitActionSignature(previous) {
		row.ActionKey, row.ActionDate = previous.ActionKey, previous.ActionDate
		return
	}
	row.ActionKey = stablePositionExitHash(struct {
		UserID, PositionID, PreviousID int64
		Signature, At                  string
	}{
		row.UserID, row.PositionID, previous.ID, signature, row.EvaluatedAt.Format(time.RFC3339Nano)})
	row.ActionDate = row.TradeDate
}

// 持仓评估和事件台账同事务提交。交接失败可在下一轮重试，租约防止并发重复交接。
func (s *PositionExitAssessmentService) dispatchExitNotices(ctx context.Context, userID int64) error {
	if s.notify == nil {
		return nil
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	var pending []model.PositionExitNotice
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND status IN ? AND (lease_until IS NULL OR lease_until < ?)",
		userID, []string{"pending", "sending"}, now).Order("id ASC").Limit(30).Find(&pending).Error; err != nil {
		return err
	}
	var failures []error
	for _, notice := range pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		claimAt := time.Now().UTC().Truncate(time.Millisecond)
		lease := claimAt.Add(2 * time.Minute)
		claimed := common.DB.WithContext(ctx).Model(&model.PositionExitNotice{}).
			Where("id = ? AND user_id = ? AND status IN ? AND (lease_until IS NULL OR lease_until < ?)", notice.ID, userID, []string{"pending", "sending"}, claimAt).
			Updates(map[string]any{"status": "sending", "lease_until": lease, "attempts": notice.Attempts + 1})
		if claimed.Error != nil {
			failures = append(failures, claimed.Error)
			continue
		}
		if claimed.RowsAffected == 0 {
			continue
		}
		status := "dispatched"
		var sendErr error
		var p model.Position
		var row model.PositionExitAssessment
		if err := common.DB.WithContext(ctx).Scopes(withActivePositionAccount).Where("id = ? AND user_id = ? AND status = ?", notice.PositionID, userID, model.PositionStatusHolding).First(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				status = "superseded"
			} else {
				sendErr = err
			}
		} else if err := common.DB.WithContext(ctx).Where("user_id = ? AND position_id = ?", userID, p.ID).Order("evaluated_at DESC, id DESC").First(&row).Error; err != nil {
			sendErr = err
		} else if row.ActionKey != notice.EventKey || !row.ShouldTodo || row.PositionStateHash != "" && row.PositionStateHash != positionRiskBasisHash(p) {
			status = "superseded"
		} else {
			enabled, err := notificationsEnabledFor(ctx, userID, model.BrowserNotifyCategoryExitRisk)
			if err != nil {
				sendErr = err
			} else if !enabled {
				status = "suppressed"
			} else {
				sendErr = s.notifyAssessment(ctx, row, p.AccountID)
			}
		}
		update := map[string]any{"status": status, "lease_until": nil, "last_error": ""}
		if sendErr != nil {
			update["status"] = "pending"
			update["lease_until"] = time.Now().UTC().Truncate(time.Millisecond).Add(time.Duration(math.Min(float64(notice.Attempts+1), 60)) * 30 * time.Second)
			update["last_error"] = "通知交接失败，等待重试"
			failures = append(failures, fmt.Errorf("持仓 %d 通知交接失败", notice.PositionID))
		}
		// 独立短预算仅回写交接结果；不在原上下文取消后继续发网络请求。
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		err := common.DB.WithContext(commitCtx).Model(&model.PositionExitNotice{}).Where("id = ? AND user_id = ? AND status = ? AND lease_until = ?", notice.ID, userID, "sending", lease).Updates(update).Error
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
