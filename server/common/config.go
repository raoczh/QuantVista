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

	// Production 是否生产环境（连真实 MySQL），由 InitConfig 计算。
	Production bool
	// AllowedOrigins 生产环境 CORS 白名单（env ALLOWED_ORIGINS 逗号分隔）。
	AllowedOrigins []string
	// TrustedProxies 反代场景的可信代理地址列表（env TRUSTED_PROXIES 逗号分隔，
	// 如 127.0.0.1）。为空则不信任任何代理头，ClientIP 直接取连接对端地址。
	TrustedProxies []string
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
	Production = isProductionEnv()
	AllowedOrigins = splitAndTrim(os.Getenv("ALLOWED_ORIGINS"))
	TrustedProxies = splitAndTrim(os.Getenv("TRUSTED_PROXIES"))

	// 生产环境必须显式设置强密钥，绝不允许用默认/占位值上线。
	// 「生产」判定为：连接真实远程库（MySQL）。本地 SQLite 开发放行，
	// 避免开发者被迫配齐密钥；容器化 MySQL 部署则强制 fail-fast。
	if isProductionEnv() {
		if isWeakSecret(SessionSecret) {
			FatalLog("SESSION_SECRET 未设置或仍为占位值，生产环境拒绝启动；请用 `openssl rand -base64 36` 生成后写入环境变量")
		}
		if isWeakSecret(EncryptionKey) {
			FatalLog("ENCRYPTION_KEY 未设置或仍为占位值，生产环境拒绝启动；请用 `openssl rand -base64 36` 生成后写入环境变量")
		}
	} else {
		// 开发环境放行，但缺失密钥必须显式告警，避免误部署时无声降级。
		if SessionSecret == "" {
			SysWarn("SESSION_SECRET 未设置，本次启动使用随机临时密钥签发 JWT（重启后需重新换发登录态）")
		}
		if EncryptionKey == "" {
			SysWarn("ENCRYPTION_KEY 未设置，LLM API Key 等敏感字段加密不可用（开发环境放行）")
		}
	}
}

// isProductionEnv 判定是否生产环境：SQL_DSN 指向真实远程库（MySQL）即视为生产。
// 选库口径共用 IsLocalDSN（database.go）。
// 注意：安全判定不看 DEBUG——误开 DEBUG 不应绕过生产密钥 fail-fast。
func isProductionEnv() bool {
	return !IsLocalDSN(os.Getenv("SQL_DSN"))
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

// splitAndTrim 按逗号切分并去空白，过滤空项（用于 ALLOWED_ORIGINS 等列表配置）。
func splitAndTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
