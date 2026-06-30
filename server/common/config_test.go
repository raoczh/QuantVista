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

	// debug 永远视为非生产
	DebugEnabled = true
	os.Setenv("SQL_DSN", "user:pass@tcp(host:3306)/db")
	if isProductionEnv() {
		t.Error("DEBUG=true 时应视为非生产")
	}

	// 非 debug + SQLite（空 / local）→ 开发
	DebugEnabled = false
	for _, dsn := range []string{"", "local", "LOCAL", " local "} {
		os.Setenv("SQL_DSN", dsn)
		if isProductionEnv() {
			t.Errorf("SQL_DSN=%q 走 SQLite，应视为开发", dsn)
		}
	}

	// 非 debug + 真实 MySQL DSN → 生产
	os.Setenv("SQL_DSN", "user:pass@tcp(172.18.0.1:3306)/quantvista")
	if !isProductionEnv() {
		t.Error("非 debug + MySQL DSN 应视为生产")
	}
}
