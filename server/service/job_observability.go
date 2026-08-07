package service

import (
	"errors"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const JobEventRetention = 30 * 24 * time.Hour

type JobMetricBucket struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type JobCapacityMetrics struct {
	Workers   int `json:"workers"`
	Capacity  int `json:"capacity"`
	InUse     int `json:"in_use"`
	Available int `json:"available"`
}

type JobRuntimeMetrics struct {
	Buckets      []JobMetricBucket  `json:"buckets"`
	OldestQueued *time.Time         `json:"oldest_queued_at,omitempty"`
	Capacity     JobCapacityMetrics `json:"capacity"`
	GeneratedAt  time.Time          `json:"generated_at"`
}

func GetJobRuntimeMetrics(actorID int64) (*JobRuntimeMetrics, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	if !isAdminUser(actorID) {
		return nil, ErrJobNotFound
	}
	metrics := &JobRuntimeMetrics{GeneratedAt: time.Now()}
	if err := common.DB.Model(&model.JobRun{}).Select("kind", "status", "COUNT(*) AS count").
		Group("kind, status").Order("kind, status").Scan(&metrics.Buckets).Error; err != nil {
		return nil, err
	}
	var oldest struct{ QueuedAt *time.Time }
	if err := common.DB.Model(&model.JobRun{}).Select("MIN(queued_at) AS queued_at").
		Where("status = ?", model.JobStatusQueued).Scan(&oldest).Error; err != nil {
		return nil, err
	}
	metrics.OldestQueued = oldest.QueuedAt
	metrics.Capacity = defaultJobRuntime.capacityMetrics()
	return metrics, nil
}

func (r *jobRuntime) capacityMetrics() JobCapacityMetrics {
	capacity := cap(r.slots)
	inUse := len(r.slots)
	return JobCapacityMetrics{Workers: r.workers, Capacity: capacity, InUse: inUse, Available: capacity - inUse}
}

// CleanupExpiredJobEvents 只删除已终态作业的过期 SSE 通知。JobRun、JobStep、请求
// 快照、业务结果和 ResearchArtifact 均不在本阶段清理，以保护重跑、审计与血缘。
func CleanupExpiredJobEvents(now time.Time) (int64, error) {
	if common.DB == nil {
		return 0, errors.New("数据库尚未初始化")
	}
	cutoff := now.Add(-JobEventRetention)
	terminalRuns := common.DB.Model(&model.JobRun{}).Select("id").Where("status IN ?",
		[]string{model.JobStatusSuccess, model.JobStatusDegraded, model.JobStatusFailed, model.JobStatusCanceled})
	res := common.DB.Where("created_at < ? AND job_run_id IN (?)", cutoff, terminalRuns).Delete(&model.JobEvent{})
	return res.RowsAffected, res.Error
}

var startJobMaintenanceOnce sync.Once

func StartJobMaintenanceJobs() {
	startJobMaintenanceOnce.Do(func() {
		go func() {
			for {
				now := time.Now()
				next := time.Date(now.Year(), now.Month(), now.Day(), 3, 35, 0, 0, now.Location())
				if !next.After(now) {
					next = next.AddDate(0, 0, 1)
				}
				time.Sleep(next.Sub(now))
				if deleted, err := CleanupExpiredJobEvents(time.Now()); err != nil {
					common.SysWarn("清理过期作业事件失败: %v", err)
				} else if deleted > 0 {
					common.SysLog("清理过期作业事件 %d 条", deleted)
				}
			}
		}()
	})
}
