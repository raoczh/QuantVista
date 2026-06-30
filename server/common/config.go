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

	// 生产环境（非 debug）必须显式设置密钥，避免用默认值上线。
	if !DebugEnabled {
		if SessionSecret == "" {
			SysWarn("SESSION_SECRET 未设置，已在非生产场景下放行；正式部署务必设置")
		}
		if EncryptionKey == "" {
			SysWarn("ENCRYPTION_KEY 未设置，API Key/敏感字段加密将不可用；正式部署务必设置")
		}
	}
}
