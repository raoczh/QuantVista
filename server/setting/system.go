// Package setting 把 DB 里的系统配置（model.Option）加载为强类型内存变量，
// 提供读写接口；写入即持久化。敏感值（GitHub secret）密文落库、内存明文。
package setting

import (
	"os"
	"strconv"
	"sync"

	"quantvista/common"
	"quantvista/model"
)

// 系统配置项 key。
const (
	keyRegistrationOpen   = "registration_open"
	keyGitHubOAuthEnabled = "github_oauth_enabled"
	keyGitHubClientID     = "github_client_id"
	keyGitHubClientSecret = "github_client_secret" // 密文存储
	keyNewsInterval       = "news_collect_interval_min"
	keyNewsAutoLLM        = "news_auto_llm"
)

// 新闻快讯采集间隔（分钟）的默认值与钳制范围：下限防打爆免费上游，上限防配成"实际不采集"。
const (
	NewsIntervalDefault = 5
	NewsIntervalMin     = 1
	NewsIntervalMax     = 120
)

var (
	mu                 sync.RWMutex
	registrationOpen   bool
	gitHubOAuthEnabled bool
	gitHubClientID     string
	gitHubClientSecret string // 内存明文，由密文解密而来
	// 新闻两项初始值即默认行为：Init 之前被读（如测试环境）也不会出现
	// 间隔 0 空转或静默关闭 LLM 的意外。
	newsIntervalMin = NewsIntervalDefault // 新闻快讯采集间隔（分钟）
	newsAutoLLM     = true                // 是否允许采集后自动调 LLM 做新闻情绪分析
)

// Init 从 DB 加载系统配置；首启时若 DB 缺 GitHub 凭证而 env 提供了，则种子回填到 DB。
func Init() error {
	opts, err := model.LoadOptions()
	if err != nil {
		return err
	}

	// env 种子（仅当 DB 尚无该值）：client_id 可放 env，secret 也支持 env 引导。
	if _, ok := opts[keyGitHubClientID]; !ok {
		if v := os.Getenv("GITHUB_CLIENT_ID"); v != "" {
			_ = model.UpsertOption(keyGitHubClientID, v)
			opts[keyGitHubClientID] = v
		}
	}
	if _, ok := opts[keyGitHubClientSecret]; !ok {
		if v := os.Getenv("GITHUB_CLIENT_SECRET"); v != "" {
			if cipher, err := common.Encrypt(v); err == nil {
				_ = model.UpsertOption(keyGitHubClientSecret, cipher)
				opts[keyGitHubClientSecret] = cipher
			}
		}
	}

	apply(opts)
	common.SysLog("系统设置已加载：注册开放=%v，GitHub 登录=%v", registrationOpen, gitHubOAuthEnabled)
	return nil
}

// apply 解析 options map 到内存变量。
func apply(opts map[string]string) {
	mu.Lock()
	defer mu.Unlock()

	registrationOpen = opts[keyRegistrationOpen] == "true"
	gitHubClientID = opts[keyGitHubClientID]

	gitHubClientSecret = ""
	if cipher := opts[keyGitHubClientSecret]; cipher != "" {
		if plain, err := common.Decrypt(cipher); err == nil {
			gitHubClientSecret = plain
		} else {
			common.SysWarn("GitHub client secret 解密失败（ENCRYPTION_KEY 是否变更？）: %v", err)
		}
	}

	// 显式开关优先；未显式设置时，凭证齐全则视为启用。
	if v, ok := opts[keyGitHubOAuthEnabled]; ok {
		gitHubOAuthEnabled = v == "true"
	} else {
		gitHubOAuthEnabled = gitHubClientID != "" && gitHubClientSecret != ""
	}

	newsIntervalMin = clampNewsInterval(opts[keyNewsInterval])
	// 默认允许（!= "false"）：升级到本版本前该 key 不存在，不能静默关掉既有的自动 LLM 行为。
	newsAutoLLM = opts[keyNewsAutoLLM] != "false"
}

// clampNewsInterval 解析并钳制采集间隔：缺失/非法回默认 5，越界钳到 [1,120]。
func clampNewsInterval(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return NewsIntervalDefault
	}
	if n < NewsIntervalMin {
		return NewsIntervalMin
	}
	if n > NewsIntervalMax {
		return NewsIntervalMax
	}
	return n
}

// ---- 读取 ----

func RegistrationOpen() bool   { mu.RLock(); defer mu.RUnlock(); return registrationOpen }
func GitHubOAuthEnabled() bool { mu.RLock(); defer mu.RUnlock(); return gitHubOAuthEnabled }
func GitHubClientID() string   { mu.RLock(); defer mu.RUnlock(); return gitHubClientID }

// GitHubClientSecret 返回内存明文密钥，仅供后端 OAuth 流程内部使用。
func GitHubClientSecret() string { mu.RLock(); defer mu.RUnlock(); return gitHubClientSecret }

// HasGitHubSecret 用于后台展示「是否已配置」而不泄露密钥本身。
func HasGitHubSecret() bool { mu.RLock(); defer mu.RUnlock(); return gitHubClientSecret != "" }

// NewsCollectIntervalMin 新闻快讯采集间隔（分钟），已钳制在 [1,120]。
func NewsCollectIntervalMin() int { mu.RLock(); defer mu.RUnlock(); return newsIntervalMin }

// NewsAutoLLM 是否允许采集后自动调 LLM 做新闻情绪增强；关闭时只做规则分析。
func NewsAutoLLM() bool { mu.RLock(); defer mu.RUnlock(); return newsAutoLLM }

// ---- 写入（持久化 + 刷新内存）----

// SetRegistrationOpen 设置是否开放注册。
func SetRegistrationOpen(v bool) error {
	if err := model.UpsertOption(keyRegistrationOpen, strconv.FormatBool(v)); err != nil {
		return err
	}
	mu.Lock()
	registrationOpen = v
	mu.Unlock()
	return nil
}

// SetNewsCollectIntervalMin 设置新闻快讯采集间隔（分钟），越界钳制到 [1,120]。
// 变更在采集 job 的下一轮生效（job 每轮结束重读本值）。
func SetNewsCollectIntervalMin(v int) error {
	if v < NewsIntervalMin {
		v = NewsIntervalMin
	}
	if v > NewsIntervalMax {
		v = NewsIntervalMax
	}
	if err := model.UpsertOption(keyNewsInterval, strconv.Itoa(v)); err != nil {
		return err
	}
	mu.Lock()
	newsIntervalMin = v
	mu.Unlock()
	return nil
}

// SetNewsAutoLLM 设置是否允许自动调 LLM 处理新闻。
func SetNewsAutoLLM(v bool) error {
	if err := model.UpsertOption(keyNewsAutoLLM, strconv.FormatBool(v)); err != nil {
		return err
	}
	mu.Lock()
	newsAutoLLM = v
	mu.Unlock()
	return nil
}

// SetGitHubOAuth 更新 GitHub 凭证与开关。secret 为空表示保留原值（后台不必重复输入密钥）。
func SetGitHubOAuth(clientID, clientSecret string, enabled bool) error {
	if err := model.UpsertOption(keyGitHubClientID, clientID); err != nil {
		return err
	}
	if clientSecret != "" {
		cipher, err := common.Encrypt(clientSecret)
		if err != nil {
			return err
		}
		if err := model.UpsertOption(keyGitHubClientSecret, cipher); err != nil {
			return err
		}
	}
	if err := model.UpsertOption(keyGitHubOAuthEnabled, strconv.FormatBool(enabled)); err != nil {
		return err
	}

	mu.Lock()
	gitHubClientID = clientID
	if clientSecret != "" {
		gitHubClientSecret = clientSecret
	}
	gitHubOAuthEnabled = enabled
	mu.Unlock()
	return nil
}
