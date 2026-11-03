package service

import (
	"encoding/json"
	"errors"

	"quantvista/model"

	"gorm.io/gorm"
)

var ErrJobMaintenancePreviewRequired = errors.New("该补采任务不能直接重跑，请返回管理后台重新预演并确认新计划后执行")

func jobKindRequiresMaintenancePlan(kind string) bool {
	switch kind {
	case JobKindSyncDailyBars, JobKindBackfillCalendar, JobKindSyncMarketWide:
		return true
	default:
		return false
	}
}

func validateMaintenanceJobRetry(kind string, raw json.RawMessage) error {
	if !jobKindRequiresMaintenancePlan(kind) {
		return nil
	}
	var request DataSyncJobRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if request.HasMaintenance {
		return ErrJobMaintenancePreviewRequired
	}
	return nil
}

// Only failed, plan-bound job kinds need an extra bounded read. Project the flag
// inside SQL so task lists never fetch request bodies or saved plan hashes.
func listMaintenanceJobRetryBlocks(database *gorm.DB, runs []model.JobRun) (map[int64]bool, error) {
	blocked := make(map[int64]bool)
	jobIDs := make([]int64, 0, len(runs))
	for _, run := range runs {
		if run.Status == model.JobStatusFailed && jobKindRequiresMaintenancePlan(run.Kind) {
			jobIDs = append(jobIDs, run.ID)
		}
	}
	if len(jobIDs) == 0 {
		return blocked, nil
	}
	var flags []struct {
		ID           int64
		NeedsPreview bool
	}
	// JSON_VALID and JSON_EXTRACT work on both supported databases. Corrupt
	// snapshots stay non-retryable instead of breaking the entire task list.
	err := database.Model(&model.JobRun{}).
		Select(`id, CASE WHEN JSON_VALID(request_snapshot)
			THEN COALESCE(JSON_EXTRACT(request_snapshot, '$.request.has_maintenance') = true, false)
			ELSE true END AS needs_preview`).
		Where("id IN ?", jobIDs).Find(&flags).Error
	if err != nil {
		return nil, err
	}
	for _, flag := range flags {
		blocked[flag.ID] = flag.NeedsPreview
	}
	return blocked, nil
}
