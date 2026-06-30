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
)

var (
	mu                 sync.RWMutex
	registrationOpen   bool
	gitHubOAuthEnabled bool
	gitHubClientID     string
	gitHubClientSecret string // 内存明文，由密文解密而来
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
}

// ---- 读取 ----

func RegistrationOpen() bool   { mu.RLock(); defer mu.RUnlock(); return registrationOpen }
func GitHubOAuthEnabled() bool { mu.RLock(); defer mu.RUnlock(); return gitHubOAuthEnabled }
func GitHubClientID() string   { mu.RLock(); defer mu.RUnlock(); return gitHubClientID }

// GitHubClientSecret 返回内存明文密钥，仅供后端 OAuth 流程内部使用。
func GitHubClientSecret() string { mu.RLock(); defer mu.RUnlock(); return gitHubClientSecret }

// HasGitHubSecret 用于后台展示「是否已配置」而不泄露密钥本身。
func HasGitHubSecret() bool { mu.RLock(); defer mu.RUnlock(); return gitHubClientSecret != "" }

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
