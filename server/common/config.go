package common

import (
	"os"
	"strconv"
	"strings"
)

// Version 由构建时 ldflags 注入（-X 'quantvista/common.Version=...'），默认 dev。
var Version = "dev"

// 全局运行期配置，启动时由 InitConfig 填充。
var (
	Port          string
	DefaultMarket string
	SessionSecret string
	EncryptionKey string

	GithubClientID     string
	GithubClientSecret string

	TushareToken string

	DebugEnabled bool
)

// GetEnv 读取环境变量，缺省回退 fallback。
func GetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func GetEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// InitConfig 从环境变量加载配置，并对安全相关项做最低限度校验。
func InitConfig() {
	Port = GetEnv("PORT", "3000")
	DefaultMarket = strings.ToLower(GetEnv("DEFAULT_MARKET", "cn"))
	SessionSecret = os.Getenv("SESSION_SECRET")
	EncryptionKey = os.Getenv("ENCRYPTION_KEY")
	GithubClientID = os.Getenv("GITHUB_CLIENT_ID")
	GithubClientSecret = os.Getenv("GITHUB_CLIENT_SECRET")
	TushareToken = os.Getenv("TUSHARE_TOKEN")
	DebugEnabled = GetEnvBool("DEBUG", false)

	// 生产环境必须显式设置强密钥，绝不允许用默认/占位值上线。
	// 「生产」判定为：非 debug 且连接真实远程库（MySQL）。本地 SQLite 开发永远放行，
	// 避免开发者被迫配齐密钥；容器化 MySQL 部署则强制 fail-fast。
	if isProductionEnv() {
		if isWeakSecret(SessionSecret) {
			FatalLog("SESSION_SECRET 未设置或仍为占位值，生产环境拒绝启动；请用 `openssl rand -base64 36` 生成后写入环境变量")
		}
		if isWeakSecret(EncryptionKey) {
			FatalLog("ENCRYPTION_KEY 未设置或仍为占位值，生产环境拒绝启动；请用 `openssl rand -base64 36` 生成后写入环境变量")
		}
	} else if EncryptionKey == "" {
		// 开发环境放行，但提示：未配置时 LLM API Key 等敏感字段加密不可用。
		SysWarn("ENCRYPTION_KEY 未设置，LLM API Key 等敏感字段加密不可用（开发环境放行）")
	}
}

// isProductionEnv 判定是否生产环境：非 debug 且 SQL_DSN 指向真实远程库。
// 与 database.go 选库口径一致——SQL_DSN 为空或 "local" 走 SQLite，视为开发。
func isProductionEnv() bool {
	if DebugEnabled {
		return false
	}
	dsn := strings.ToLower(strings.TrimSpace(os.Getenv("SQL_DSN")))
	return dsn != "" && dsn != "local"
}

// isWeakSecret 判定密钥是否为空或仍是模板占位值，生产环境一律拒绝。
func isWeakSecret(v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	lower := strings.ToLower(v)
	// 命中 .env.example 的占位前缀/关键词即视为「未真正配置」。
	for _, bad := range []string{"please-", "your-", "change-me", "changeme", "random-secret"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}
