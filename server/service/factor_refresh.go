package service

import (
	"context"
	"errors"
	"time"
)

var errFactorInputsChanged = errors.New("日线数据已更新，正在重新计算因子，请稍后重试")

func invalidateFactorTable() {
	factorTableMu.Lock()
	factorInputRevision++
	factorTableCur = nil
	factorTableMu.Unlock()
	factorFreshMu.Lock()
	factorFreshVal = ""
	factorFreshMu.Unlock()
}

func publishFactorTable(table *FactorTable) error {
	factorTableMu.Lock()
	defer factorTableMu.Unlock()
	if table.inputRevision != factorInputRevision {
		return errFactorInputsChanged
	}
	factorTableCur = table
	return nil
}

func rebuildFactorTableForJob(ctx context.Context, reason string) (*FactorTable, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		table, err := RebuildFactorTable(ctx, reason)
		if !errors.Is(err, ErrSyncInProgress) && !errors.Is(err, errFactorInputsChanged) {
			return table, err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Only committed maintenance writes trigger this. A canceled/partial task may
// still have saved valid history, so completion status alone is not the signal.
func refreshFactorsAfterMaintenance(ctx context.Context, reason string) {
	invalidateFactorTable()
	if _, inJob := currentJobExecution(ctx); inJob {
		ScheduleSystemDataSyncJob(JobKindFactorRebuild, DataSyncJobRequest{
			Version: dataSyncJobSnapshotVersion, Market: "cn", TriggerSource: "system", Reason: reason,
		})
	} else {
		RebuildFactorTableAsync(reason)
	}
}
