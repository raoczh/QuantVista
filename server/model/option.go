package model

import (
	"context"
	"errors"

	"quantvista/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Option 系统级配置 KV（注册开关、GitHub 凭证等）。值统一以字符串存储，
// 由 setting 包加载为强类型内存变量；敏感值（如 GitHub secret）密文存储。
type Option struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `gorm:"type:text" json:"value"`
}

// LoadOptions 读取全部系统配置。
func LoadOptions() (map[string]string, error) {
	var rows []Option
	if err := common.DB.Find(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// UpsertOption 写入/更新单个配置项。
func UpsertOption(key, value string, contexts ...context.Context) error {
	db := common.DB
	if len(contexts) > 0 && contexts[0] != nil {
		db = db.WithContext(contexts[0])
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).
		Create(&Option{Key: key, Value: value}).Error
}

// LoadOption 只把确实不存在解释为空，存储故障和取消均交给调用方。
func LoadOption(ctx context.Context, key string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var row Option
	err := common.DB.WithContext(ctx).Where("`key` = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return row.Value, err
}
