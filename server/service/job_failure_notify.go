package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const jobFailureMergeWindow = 5 * time.Minute

type jobFailureNoticeClaim struct {
	Notice model.JobFailureNotification
	Send   bool
}

func jobFailureGroupKey(userID int64, kind string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", userID, kind)))
	return hex.EncodeToString(sum[:])
}

func safeJobNoticeCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 64 {
		return "failed"
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "failed"
	}
	return code
}

func jobFailureSummary(code string) string {
	return "任务执行失败（错误码 " + safeJobNoticeCode(code) + "）"
}

func reserveJobFailureNotice(run model.JobRun, enabled bool, now time.Time) (jobFailureNoticeClaim, error) {
	var claim jobFailureNoticeClaim
	if run.OwnerType != model.JobOwnerUser || run.OwnerUserID == nil || *run.OwnerUserID <= 0 || run.Status != model.JobStatusFailed {
		return claim, nil
	}
	userID := *run.OwnerUserID
	code := safeJobNoticeCode(run.ErrorCode)
	summary := jobFailureSummary(code)

	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var existing model.JobFailureNotification
		if err := tx.Where("job_run_id = ?", run.ID).First(&existing).Error; err == nil {
			claim.Notice = existing
			return nil
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if !enabled {
			notice := model.JobFailureNotification{
				JobRunID: run.ID, UserID: userID, Kind: run.Kind, ErrorCode: code,
				ErrorSummary: summary, Status: model.JobFailureNoticeSuppressed, MergeCount: 1,
				CreatedAt: now, UpdatedAt: now,
			}
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&notice)
			if res.Error != nil {
				return res.Error
			}
			claim.Notice = notice
			return nil
		}

		mergeInto := func(root model.JobFailureNotification) error {
			merged := model.JobFailureNotification{
				JobRunID: run.ID, UserID: userID, Kind: run.Kind, MergeRootID: &root.ID,
				ErrorCode: code, ErrorSummary: summary, Status: model.JobFailureNoticeMerged, MergeCount: 1,
				CreatedAt: now, UpdatedAt: now,
			}
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&merged)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 1 {
				if err := tx.Model(&model.JobFailureNotification{}).Where("id = ?", root.ID).
					UpdateColumn("merge_count", gorm.Expr("merge_count + 1")).Error; err != nil {
					return err
				}
			}
			claim.Notice = merged
			return nil
		}

		groupKey := jobFailureGroupKey(userID, run.Kind)
		// 先取真实滑动窗口，避免固定桶边界把相邻失败拆成两次通知。
		var recentRoot model.JobFailureNotification
		recent := tx.Where("group_key = ? AND created_at >= ?", groupKey, now.Add(-jobFailureMergeWindow)).
			Order("created_at DESC, id DESC").First(&recentRoot)
		if recent.Error == nil {
			return mergeInto(recentRoot)
		}
		if !errors.Is(recent.Error, gorm.ErrRecordNotFound) {
			return recent.Error
		}

		// 每个用户+kind 只保留一个当前并发组键。窗口过期时先释放旧 root，
		// 再由唯一键串行化多实例同时创建的新 root。
		if err := tx.Model(&model.JobFailureNotification{}).Where("group_key = ?", groupKey).
			Update("group_key", nil).Error; err != nil {
			return err
		}
		root := model.JobFailureNotification{
			JobRunID: run.ID, UserID: userID, Kind: run.Kind, GroupKey: &groupKey,
			ErrorCode: code, ErrorSummary: summary, Status: model.JobFailureNoticeClaimed, MergeCount: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&root)
		if res.Error != nil {
			return res.Error
		}
		// 不依赖 MySQL/SQLite 对 ON CONFLICT 的 RowsAffected 差异；按 JobRun 唯一键
		// 回读后才能确认本次失败是否真的拥有一条 root 事实。
		if err := tx.Where("job_run_id = ?", run.ID).First(&existing).Error; err == nil {
			claim.Notice = existing
			claim.Send = existing.Status == model.JobFailureNoticeClaimed
			return nil
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		// 没有本 JobRun 行，说明唯一冲突来自同一短窗的另一个同类失败。
		if err := tx.Where("group_key = ?", groupKey).First(&root).Error; err != nil {
			return err
		}
		return mergeInto(root)
	})
	return claim, err
}

// notifyFailedJob 只消费已经提交的 failed JobRun。幂等声明先提交，再 best-effort 外发，
// 因而 CAS 重试、恢复扫描或重复观察都不会导致二次通知。
func (r *jobRuntime) notifyFailedJob(jobID int64) {
	if common.DB == nil {
		return
	}
	var run model.JobRun
	if err := common.DB.Where("id = ? AND status = ?", jobID, model.JobStatusFailed).First(&run).Error; err != nil {
		return
	}
	if run.OwnerType != model.JobOwnerUser || run.OwnerUserID == nil || *run.OwnerUserID <= 0 {
		return
	}
	userID := *run.OwnerUserID
	enabled := false
	if r.notifier != nil {
		var err error
		enabled, err = alertNotificationsEnabled(context.Background(), userID)
		if err != nil {
			common.SysWarn("读取任务失败通知开关失败 job=%d: %v", jobID, err)
			return
		}
	}
	claim, err := reserveJobFailureNotice(run, enabled, time.Now())
	if err != nil {
		common.SysWarn("声明任务失败通知失败 job=%d: %v", jobID, err)
		return
	}
	if !claim.Send || r.notifier == nil {
		return
	}

	attemptedAt := time.Now()
	claimed := common.DB.Model(&model.JobFailureNotification{}).
		Where("id = ? AND status = ?", claim.Notice.ID, model.JobFailureNoticeClaimed).
		Updates(map[string]any{"status": model.JobFailureNoticeDispatching, "attempted_at": &attemptedAt})
	if claimed.Error != nil {
		common.SysWarn("抢占任务失败通知投递权失败 job=%d: %v", jobID, claimed.Error)
		return
	}
	if claimed.RowsAffected != 1 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), alertNotifyFlushTimeout)
	defer cancel()
	route := fmt.Sprintf("/tasks?job_id=%d", run.ID)
	content := fmt.Sprintf("任务类型：%s\n%s\n同类短时失败会自动合并，请在任务中心查看恢复建议。\n查看详情：%s",
		safeAlertContextText(run.Kind, 64), claim.Notice.ErrorSummary, route)
	r.notifier.SendMsgContext(ctx, userID, NotifyMessage{
		Title: "QuantVista 任务失败", Content: content, Route: route,
		Kind: NotifyMsgKindTaskFailure, Priority: 4,
	})
	// NotifyService 逐通道 best-effort 且不返回聚合结果；这里只能确认已尝试，
	// 不能把通道实际成功与否伪记为 dispatched。
	if err := common.DB.Model(&model.JobFailureNotification{}).
		Where("id = ? AND status = ?", claim.Notice.ID, model.JobFailureNoticeDispatching).
		Update("status", model.JobFailureNoticeAttempted).Error; err != nil {
		common.SysWarn("任务失败通知投递事实回写失败 job=%d: %v", jobID, err)
	}
}
