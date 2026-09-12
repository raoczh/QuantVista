package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupEnvironmentMissingAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.env")
	if err := loadEnvironmentFiles(missing); err != nil {
		t.Fatal(err)
	}
	const inherited = "QV_REVIEW_STARTUP_INHERITED"
	const selected = "QV_REVIEW_STARTUP_SELECTED"
	t.Setenv(inherited, "process-value")
	t.Setenv(selected, "")
	if err := os.Unsetenv(selected); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(dir, "first.env"), filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte(inherited+"=file-value\n"+selected+"=first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(selected+"=second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvironmentFiles(missing, first, second); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(inherited) != "process-value" || os.Getenv(selected) != "first" {
		t.Fatal("应保留进程环境优先级，只读取首个存在的文件")
	}
}

func TestStartupEnvironmentFailureStopsWithoutLeakingValues(t *testing.T) {
	dir := t.TempDir()
	invalid, fallback := filepath.Join(dir, "invalid.env"), filepath.Join(dir, "fallback.env")
	const key = "QV_REVIEW_STARTUP_FALLBACK"
	const secret = "qv-review-private-sentinel"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(key+"=\""+secret), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte(key+"=fallback\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := loadEnvironmentFiles(invalid, fallback)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "invalid.env") {
		t.Fatalf("必须中止且不泄露文件值: %v", err)
	}
	if _, exists := os.LookupEnv(key); exists {
		t.Fatal("损坏首选配置后不能加载后备文件")
	}
	if err := loadEnvironmentFiles(dir, fallback); err == nil {
		t.Fatal("目录不能当成可忽略的环境文件")
	}
}
