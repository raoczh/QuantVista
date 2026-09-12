package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/oauth"
	"quantvista/setting"

	"gorm.io/gorm"
)

type identityReviewTransport func(*http.Request) (*http.Response, error)

func (f identityReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGitHubInitializationStorageFailureCannotCreateOrdinaryFirstUser(t *testing.T) {
	setupTestDB(t)
	registration := setting.RegistrationOpen()
	if err := setting.SetRegistrationOpen(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setting.SetRegistrationOpen(registration) })
	fault := errors.New("初始化标记暂时写入失败")
	const callback = "review_identity_initialization_failure"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "options" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
	_, err := NewAuthService().userForGitHub(&oauth.GitHubUser{GithubID: "987654", Username: "initialization-failure"})
	if !errors.Is(err, fault) {
		t.Fatalf("存储故障不能误判为并发初始化完成：%v", err)
	}
	var users int64
	if err := common.DB.Model(&model.User{}).Count(&users).Error; err != nil || users != 0 {
		t.Fatalf("失败事务不得创建没有管理员的首用户：users=%d err=%v", users, err)
	}
}

func TestGitHubConcurrentBindingHasSingleOwner(t *testing.T) {
	setupTestDB(t)
	oldEncryptionKey := common.EncryptionKey
	common.EncryptionKey = "review-followup-local-encryption-key"
	t.Cleanup(func() { common.EncryptionKey = oldEncryptionKey })
	users := []model.User{
		{ID: 951, Username: "review-oauth-a", Password: "already-set", Status: model.StatusEnabled},
		{ID: 952, Username: "review-oauth-b", Password: "already-set", Status: model.StatusEnabled},
	}
	if err := common.DB.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	oldID, oldSecret, oldEnabled := setting.GitHubClientID(), setting.GitHubClientSecret(), setting.GitHubOAuthEnabled()
	if err := setting.SetGitHubOAuth("local-test-client", "local-test-secret", true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setting.SetGitHubOAuth(oldID, oldSecret, oldEnabled) })
	previousTransport := http.DefaultTransport
	http.DefaultTransport = identityReviewTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Host + r.URL.Path {
		case "github.com/login/oauth/access_token":
			body = `{"access_token":"local-test-token"}`
		case "api.github.com/user":
			body = `{"id":777777,"login":"same-local-github"}`
		default:
			return nil, fmt.Errorf("unexpected external request: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	// 两个真实 BindGitHub 请求均完成判重读取后再允许继续，固定可发生的并发顺序。
	var arrived atomic.Int32
	var releaseOnce sync.Once
	release := make(chan struct{})
	const callback = "identity_review_binding_duplicate_barrier"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		query := tx.Statement.SQL.String()
		if tx.Statement.Table != "users" || !strings.Contains(query, "count(") || !strings.Contains(query, "github_id") {
			return
		}
		if arrived.Add(1) == 2 {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			tx.AddError(fmt.Errorf("判重屏障超时"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		common.DB.Callback().Query().Remove(callback)
	})
	results := make(chan error, 2)
	for _, user := range users {
		go func(id int64) {
			_, err := NewAuthService().BindGitHub(context.Background(), id, "local-code", common.SignState(), "https://example.com/login/callback")
			results <- err
		}(user.ID)
	}
	first, second := <-results, <-results
	if arrived.Load() != 2 {
		t.Fatalf("未进入两次判重读取：%d，errors=%v/%v", arrived.Load(), first, second)
	}
	common.DB.Callback().Query().Remove(callback)
	var count int64
	if err := common.DB.Model(&model.User{}).Where("github_id = ?", "777777").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	resolved, err := NewAuthService().userForGitHub(&oauth.GitHubUser{GithubID: "777777", Username: "same-local-github"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || (first == nil) == (second == nil) {
		t.Fatalf("同一 GitHub 身份绑定到 %d 个本地账号；两次绑定错误=%v/%v；后续登录固定落入账号 %d", count, first, second, resolved.ID)
	}
}

func TestCreateAdminSessionFailureRollsBackInitialization(t *testing.T) {
	setupTestDB(t)
	fault := errors.New("首启会话写入暂时失败")
	const callback = "review_initial_admin_session_failure"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "refresh_tokens" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	svc := NewAuthService()
	if _, err := svc.CreateAdmin("review-first-admin", "local-review-password", "review"); !errors.Is(err, fault) {
		t.Fatalf("必须如实返回会话创建故障：%v", err)
	}
	if need, err := svc.SetupNeeded(); err != nil || !need {
		t.Errorf("失败的首启请求不能留下已初始化状态：need=%v err=%v", need, err)
	}
	var initialized int64
	if err := common.DB.Model(&model.Option{}).Where("`key` = ?", "initialized").Count(&initialized).Error; err != nil || initialized != 0 {
		t.Errorf("初始化闸必须与用户和会话一起回滚：count=%d err=%v", initialized, err)
	}
	_ = common.DB.Callback().Create().Remove(callback)
	pair, err := svc.CreateAdmin("review-first-admin", "local-review-password", "review-retry")
	if err != nil || pair == nil || pair.User == nil || pair.User.Password != "" || pair.RefreshToken == "" {
		t.Fatalf("故障恢复后原始首启操作应可重试：pair=%v err=%v", pair != nil, err)
	}
}
