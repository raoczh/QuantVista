package common

import (
	"os"
	"testing"
)

func TestIsWeakSecret(t *testing.T) {
	weak := []string{
		"",
		"   ",
		"please-generate-a-random-secret",
		"please-generate-another-random-secret",
		"your-github-oauth-app-client-id",
		"please-change-me",
		"ChangeMe123",
	}
	for _, v := range weak {
		if !isWeakSecret(v) {
			t.Errorf("isWeakSecret(%q) 应为 true（弱/占位值）", v)
		}
	}
	strong := []string{
		"k9Xa2bQ7mZ0pLrT5",
		"aGVsbG8td29ybGQtc2VjcmV0LXZhbHVl",
	}
	for _, v := range strong {
		if isWeakSecret(v) {
			t.Errorf("isWeakSecret(%q) 应为 false（已是真实密钥）", v)
		}
	}
}

func TestIsProductionEnv(t *testing.T) {
	orig := os.Getenv("SQL_DSN")
	origDebug := DebugEnabled
	defer func() { os.Setenv("SQL_DSN", orig); DebugEnabled = origDebug }()

	// 安全判定与 DEBUG 解耦：误开 DEBUG 不得绕过生产密钥 fail-fast
	DebugEnabled = true
	os.Setenv("SQL_DSN", "user:pass@tcp(host:3306)/db")
	if !isProductionEnv() {
		t.Error("连真实 MySQL 时即使 DEBUG=true 也应视为生产（不绕过密钥校验）")
	}

	// SQLite（空 / local，容忍大小写与空白）→ 开发
	DebugEnabled = false
	for _, dsn := range []string{"", "local", "LOCAL", " local "} {
		os.Setenv("SQL_DSN", dsn)
		if isProductionEnv() {
			t.Errorf("SQL_DSN=%q 走 SQLite，应视为开发", dsn)
		}
	}

	// 真实 MySQL DSN → 生产
	os.Setenv("SQL_DSN", "user:pass@tcp(172.18.0.1:3306)/quantvista")
	if !isProductionEnv() {
		t.Error("MySQL DSN 应视为生产")
	}
}

func TestIsLocalDSN(t *testing.T) {
	for _, dsn := range []string{"", "local", "LOCAL", " local ", "Local"} {
		if !IsLocalDSN(dsn) {
			t.Errorf("IsLocalDSN(%q) 应为 true", dsn)
		}
	}
	// 以 local 开头的真实 MySQL DSN（如用户名 local_admin）不得误判为本地库
	for _, dsn := range []string{"local_admin:pw@tcp(10.0.0.1:3306)/qv", "user:pass@tcp(host:3306)/db"} {
		if IsLocalDSN(dsn) {
			t.Errorf("IsLocalDSN(%q) 应为 false", dsn)
		}
	}
}
