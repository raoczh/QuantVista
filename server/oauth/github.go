// Package oauth 实现第三方登录提供方。当前仅 GitHub。
// 凭证从 setting 包读取（DB 系统设置，可由管理员后台配置）。
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"quantvista/setting"
)

const (
	authorizeURL = "https://github.com/login/oauth/authorize"
	tokenURL     = "https://github.com/login/oauth/access_token"
	userAPI      = "https://api.github.com/user"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

// ErrOAuthDisabled GitHub 登录未启用或凭证未配置。
var ErrOAuthDisabled = errors.New("GitHub 登录未启用或凭证未配置")

// GitHubUser 归一化后的 GitHub 用户信息。
type GitHubUser struct {
	GithubID    string // 数值 id 转字符串，永久标识
	Username    string
	DisplayName string
	Email       string
	AvatarURL   string
}

// AuthorizeURL 拼装 GitHub 授权跳转地址。redirectURI 为前端回调页（如 https://site/login/callback）。
func AuthorizeURL(state, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", setting.GitHubClientID())
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	q.Set("allow_signup", "false")
	return authorizeURL + "?" + q.Encode()
}

type tokenResp struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ExchangeToken 用授权码换 access_token。redirectURI 必须与授权时一致。
func ExchangeToken(ctx context.Context, code, redirectURI string) (string, error) {
	if !setting.GitHubOAuthEnabled() || setting.GitHubClientID() == "" || setting.GitHubClientSecret() == "" {
		return "", ErrOAuthDisabled
	}
	if code == "" {
		return "", errors.New("授权码为空")
	}

	payload, _ := json.Marshal(map[string]string{
		"client_id":     setting.GitHubClientID(),
		"client_secret": setting.GitHubClientSecret(),
		"code":          code,
		"redirect_uri":  redirectURI,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("换取 token 失败: %w", err)
	}
	defer res.Body.Close()

	var tr tokenResp
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if tr.Error != "" {
		return "", fmt.Errorf("GitHub 返回错误: %s %s", tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return "", errors.New("GitHub 未返回 access_token")
	}
	return tr.AccessToken, nil
}

type githubUserResp struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// GetUser 用 access_token 拉取 GitHub 用户信息。
func GetUser(ctx context.Context, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取用户信息失败: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 用户接口返回 http %d", res.StatusCode)
	}

	var u githubUserResp
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %w", err)
	}
	if u.ID == 0 {
		return nil, errors.New("GitHub 用户 id 为空")
	}
	return &GitHubUser{
		GithubID:    strconv.FormatInt(u.ID, 10),
		Username:    u.Login,
		DisplayName: u.Name,
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
	}, nil
}
