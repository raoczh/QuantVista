package model

import (
	"errors"
	"gorm.io/gorm"
	"time"
)

// RankingModelArtifact 只保存独立研究产出的不可变模型与评估依据；创建不会自动启用。
type RankingModelArtifact struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	RecType      string    `gorm:"size:16;index" json:"rec_type"`
	Profile      string    `gorm:"size:16;index" json:"profile"`
	Horizon      int       `json:"horizon"`
	Target       string    `gorm:"size:8" json:"target"`
	Version      string    `gorm:"size:16" json:"version"`
	DatasetHash  string    `gorm:"size:64" json:"dataset_hash"`
	ArtifactHash string    `gorm:"size:64;uniqueIndex" json:"artifact_hash"`
	Eligible     bool      `json:"eligible"`
	AsOf         string    `gorm:"size:10" json:"as_of"`
	Payload      string    `gorm:"type:mediumtext" json:"-"`
	CreatedBy    int64     `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

func (*RankingModelArtifact) BeforeUpdate(*gorm.DB) error {
	return errors.New("学习模型工件不可改写，请保存新的研究版本")
}
func (*RankingModelArtifact) BeforeDelete(*gorm.DB) error {
	return errors.New("学习模型工件须保留用于历史回放")
}

// RankingScoringPolicy 对未来新批次生效。已接受的任务使用自己的算法与模型快照。
type RankingScoringPolicy struct {
	Key        string    `gorm:"primaryKey;size:48" json:"key"`
	RecType    string    `gorm:"size:16" json:"rec_type"`
	Profile    string    `gorm:"size:16" json:"profile"`
	Algorithm  string    `gorm:"size:24" json:"algorithm"`
	ArtifactID int64     `json:"artifact_id"`
	Revision   int64     `json:"revision"`
	UpdatedBy  int64     `json:"updated_by"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type RankingPolicyAudit struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	PolicyKey  string    `gorm:"size:48;index" json:"policy_key"`
	BeforeJSON string    `gorm:"type:text" json:"before_json"`
	AfterJSON  string    `gorm:"type:text" json:"after_json"`
	ActorID    int64     `json:"actor_id"`
	CreatedAt  time.Time `json:"created_at"`
}
