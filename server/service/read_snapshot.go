package service

import (
	"context"
	"database/sql"

	"quantvista/common"

	"gorm.io/gorm"
)

// 多表纯读取共享同一个快照。所有查询均使用普通一致性读，不混入当前读或写入。
func readSnapshotTx(ctx context.Context, read func(*gorm.DB) error) error {
	opts := &sql.TxOptions{}
	if common.DB.Dialector.Name() == "mysql" {
		opts.Isolation, opts.ReadOnly = sql.LevelRepeatableRead, true
	}
	err := common.DB.WithContext(ctx).Transaction(read, opts)
	// MySQL 中断阻塞查询时偶尔先报告 bad connection / transaction done，
	// 保留调用方的取消原因，避免把主动取消误报为数据库故障。
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
