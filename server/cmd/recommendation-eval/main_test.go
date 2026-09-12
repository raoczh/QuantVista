package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCLIRequiresExistingExplicitReadOnlyInput(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err == nil {
		t.Fatal("不能隐式读取业务数据库")
	}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if err := run([]string{"-sqlite", missing}, &out); err == nil {
		t.Fatal("不存在的输入应报错")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("只读入口不能自动创建输入库")
	}
	path := filepath.Join(t.TempDir(), "输入 库#.sqlite")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE fixture (value INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"-sqlite", path}, &out); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Version        string `json:"version"`
		PromotionReady bool   `json:"promotion_ready"`
		Reason         string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != "rr1" || report.PromotionReady || report.Reason == "" {
		t.Fatal("空数据必须显示样本不足")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("CLI 读取不得改写输入文件")
	}
}
