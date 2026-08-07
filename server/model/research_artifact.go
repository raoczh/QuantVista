package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ResearchArtifact 是不可变的研究结果索引。正文、候选池和数据快照仍由原业务表
// 承载；这里仅保存可验证的内容哈希、血缘引用和原始存储定位。
type ResearchArtifact struct {
	ID            int64      `gorm:"primaryKey" json:"id"`
	Type          string     `gorm:"size:32;not null;uniqueIndex:idx_artifact_job_type,priority:2;index" json:"type"`
	SchemaVersion int        `gorm:"not null;default:1" json:"schema_version"`
	Subject       string     `gorm:"size:256;not null" json:"subject"`
	AsOf          *time.Time `gorm:"index" json:"as_of,omitempty"`
	AvailableAt   time.Time  `gorm:"not null;index" json:"available_at"`
	ContentHash   string     `gorm:"size:64;not null;index:idx_artifact_storage_hash,priority:2" json:"content_hash"`
	SourceRefs    string     `gorm:"type:text;not null" json:"source_refs"`
	StorageRef    string     `gorm:"size:128;not null;index:idx_artifact_storage_hash,priority:1" json:"storage_ref"`
	JobRunID      int64      `gorm:"not null;uniqueIndex:idx_artifact_job_type,priority:1" json:"job_run_id"`
	OwnerType     string     `gorm:"size:16;not null;index:idx_artifact_owner_created,priority:1" json:"owner"`
	OwnerUserID   *int64     `gorm:"column:owner_user_id;index" json:"owner_user_id,omitempty"`
	CreatedAt     time.Time  `gorm:"not null;index:idx_artifact_owner_created,priority:2" json:"created_at"`
}

func (a *ResearchArtifact) BeforeCreate(_ *gorm.DB) error {
	if a.Type == "" || a.SchemaVersion <= 0 || a.Subject == "" || a.ContentHash == "" ||
		a.SourceRefs == "" || a.StorageRef == "" || a.JobRunID <= 0 || a.AvailableAt.IsZero() {
		return errors.New("ResearchArtifact 缺少必填索引字段")
	}
	if a.OwnerType == JobOwnerUser {
		if a.OwnerUserID == nil || *a.OwnerUserID <= 0 {
			return errors.New("用户 ResearchArtifact 必须包含正数 owner_user_id")
		}
		return nil
	}
	if a.OwnerType != JobOwnerSystem || a.OwnerUserID != nil {
		return errors.New("无效的 ResearchArtifact owner")
	}
	return nil
}

func (a *ResearchArtifact) BeforeUpdate(_ *gorm.DB) error {
	return errors.New("ResearchArtifact 不可修改")
}

func (a *ResearchArtifact) BeforeDelete(_ *gorm.DB) error {
	return errors.New("ResearchArtifact 不可删除")
}
