package model

import "quantvista/common"

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
func UpsertOption(key, value string) error {
	return common.DB.Save(&Option{Key: key, Value: value}).Error
}
