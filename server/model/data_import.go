package model

import "time"

const (
	ImportKindWatchlist = "watchlist"
	ImportKindPosition  = "position"
	ImportKindTrade     = "trade"

	ImportStatusUploaded   = "uploaded"
	ImportStatusPreviewed  = "previewed"
	ImportStatusConfirmed  = "confirmed"
	ImportStatusRolledBack = "rolled_back"

	ImportRowValid    = "valid"
	ImportRowError    = "error"
	ImportRowConflict = "conflict"
)

// ImportBatch 是统一导入的审计根事实。原文件不落盘；CSV 表头与每行原始单元格保存在
// ImportRow.RawJSON，映射预检后再冻结 NormalizedJSON，确认接口不接收前端解析结果。
type ImportBatch struct {
	ID     string `gorm:"primaryKey;size:36" json:"id"`
	UserID int64  `gorm:"index;uniqueIndex:idx_import_dedupe,priority:1" json:"user_id"`
	Kind   string `gorm:"size:16;index;uniqueIndex:idx_import_dedupe,priority:2" json:"kind"`

	SchemaVersion int    `gorm:"default:1" json:"schema_version"`
	Attempt       int    `gorm:"not null;default:1;uniqueIndex:idx_import_dedupe,priority:4" json:"attempt"`
	Version       int    `gorm:"default:1" json:"version"`
	Status        string `gorm:"size:16;index" json:"status"`
	FileName      string `gorm:"size:255" json:"file_name"`
	FileDigest    string `gorm:"size:64;uniqueIndex:idx_import_dedupe,priority:3" json:"file_digest"`
	HeaderJSON    string `gorm:"type:text" json:"-"`
	MappingJSON   string `gorm:"type:text" json:"-"`
	MappingDigest string `gorm:"size:64" json:"mapping_digest"`
	TargetGroupID int64  `gorm:"index" json:"target_group_id"`

	TotalRows    int `json:"total_rows"`
	ValidRows    int `json:"valid_rows"`
	ErrorRows    int `json:"error_rows"`
	ConflictRows int `json:"conflict_rows"`
	CreatedRows  int `json:"created_rows"`
	UpdatedRows  int `json:"updated_rows"`

	ConfirmedAt  *time.Time `json:"confirmed_at,omitempty"`
	RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ImportRow 固化上传时的原始行和预检结论。RawJSON/NormalizedJSON 只在本人批次详情中
// 以经过裁剪的结构化字段返回，绝不进入普通列表或日志。
type ImportRow struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	UserID    int64  `gorm:"index;uniqueIndex:idx_import_row,priority:1" json:"-"`
	BatchID   string `gorm:"size:36;index;uniqueIndex:idx_import_row,priority:2" json:"-"`
	RowNumber int    `gorm:"uniqueIndex:idx_import_row,priority:3" json:"row"`
	RowDigest string `gorm:"size:64;index" json:"row_digest"`
	RawJSON   string `gorm:"type:text" json:"-"`

	Status         string `gorm:"size:16;index" json:"status"`
	ErrorCode      string `gorm:"size:32" json:"error_code,omitempty"`
	Message        string `gorm:"size:255" json:"message,omitempty"`
	NormalizedJSON string `gorm:"type:text" json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ImportRowClaim 是已确认行的数据库级幂等声明。唯一键不包含 batch_id，确保不同文件、
// 不同映射或并发确认也不能重复写入同一业务行；批次成功回滚后删除声明，审计行仍保留。
type ImportRowClaim struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	UserID    int64     `gorm:"uniqueIndex:idx_import_claim,priority:1" json:"user_id"`
	Kind      string    `gorm:"size:16;uniqueIndex:idx_import_claim,priority:2" json:"kind"`
	RowDigest string    `gorm:"size:64;uniqueIndex:idx_import_claim,priority:3" json:"row_digest"`
	BatchID   string    `gorm:"size:36;index" json:"batch_id"`
	RowNumber int       `json:"row"`
	CreatedAt time.Time `json:"created_at"`
}

// ImportEffect 只记录本批实际创建或改写的业务记录，是可审计回滚的边界。
// position_update 的 BeforeJSON 保存导入前账本聚合态和最后流水 ID；AfterHash 用于发现
// 人工编辑、后台折算或后续交易。position/watchlist create 同样必须通过指纹检查才可删除。
type ImportEffect struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	UserID     int64     `gorm:"index;uniqueIndex:idx_import_effect,priority:1" json:"user_id"`
	BatchID    string    `gorm:"size:36;index;uniqueIndex:idx_import_effect,priority:2" json:"batch_id"`
	RowNumber  int       `gorm:"index" json:"row"`
	RecordKind string    `gorm:"size:24;uniqueIndex:idx_import_effect,priority:3" json:"record_kind"`
	RecordID   int64     `gorm:"uniqueIndex:idx_import_effect,priority:4" json:"record_id"`
	ParentID   int64     `gorm:"index" json:"parent_id"`
	Action     string    `gorm:"size:16" json:"action"`
	BeforeJSON string    `gorm:"type:text" json:"-"`
	AfterHash  string    `gorm:"size:64" json:"after_hash"`
	CreatedAt  time.Time `json:"created_at"`
}
