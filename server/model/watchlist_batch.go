package model

import "time"

const (
	WatchlistBatchApplied      = "applied"
	WatchlistBatchUndone       = "undone"
	WatchlistBatchUndoConflict = "undo_conflict"

	WatchlistBatchItemCreated  = "created"
	WatchlistBatchItemExisted  = "existed"
	WatchlistBatchItemFailed   = "failed"
	WatchlistBatchItemRemoved  = "removed"
	WatchlistBatchItemConflict = "conflict"
)

// WatchlistBatch 固化一次扫描结果批量加入自选的请求和结果摘要。RequestHash 按用户、
// 扫描结果、目标分组和去重排序后的代码生成，重复请求返回同一批次。
type WatchlistBatch struct {
	ID          string     `gorm:"primaryKey;size:36" json:"id"`
	UserID      int64      `gorm:"index;uniqueIndex:idx_watch_batch_request,priority:1" json:"user_id"`
	ResultID    int64      `gorm:"index" json:"result_id"`
	GroupID     int64      `gorm:"index" json:"group_id"`
	RequestHash string     `gorm:"size:64;uniqueIndex:idx_watch_batch_request,priority:2" json:"request_hash"`
	Status      string     `gorm:"size:16;index" json:"status"`
	Requested   int        `json:"requested"`
	Created     int        `json:"created"`
	Existed     int        `json:"existed"`
	Failed      int        `json:"failed"`
	Removed     int        `json:"removed"`
	Conflicts   int        `json:"conflicts"`
	UndoneAt    *time.Time `json:"undone_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// WatchlistBatchItem 保存逐标的事实。只有 Status=created 且当前条目指纹仍等于 AfterHash
// 时允许撤销；existed/failed 从不删除用户原有数据。
type WatchlistBatchItem struct {
	ID              int64     `gorm:"primaryKey" json:"id"`
	UserID          int64     `gorm:"index;uniqueIndex:idx_watch_batch_item,priority:1" json:"user_id"`
	BatchID         string    `gorm:"size:36;index;uniqueIndex:idx_watch_batch_item,priority:2" json:"batch_id"`
	Symbol          string    `gorm:"size:16;uniqueIndex:idx_watch_batch_item,priority:3" json:"symbol"`
	Market          string    `gorm:"size:8" json:"market"`
	Name            string    `gorm:"size:64" json:"name"`
	WatchlistItemID int64     `gorm:"index" json:"watchlist_item_id"`
	Status          string    `gorm:"size:16;index" json:"status"`
	ErrorCode       string    `gorm:"size:32" json:"error_code,omitempty"`
	Message         string    `gorm:"size:255" json:"message,omitempty"`
	AfterHash       string    `gorm:"size:64" json:"after_hash,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}
