package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

func lifecycleUser(t *testing.T) (*model.User, *TokenPair) {
	t.Helper()
	setupTestDB(t)
	hash, err := common.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	u := &model.User{Username: "lifecycle-user", Password: hash, Role: model.RoleUser, Status: model.StatusEnabled}
	if err := common.DB.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	pair, err := NewAuthService().issueFor(u, "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, pair
}

func TestRefreshRotationRollsBackOnStorageFailure(t *testing.T) {
	_, pair := lifecycleUser(t)
	if err := common.DB.Exec("CREATE TRIGGER deny_refresh_insert BEFORE INSERT ON refresh_tokens BEGIN SELECT RAISE(ABORT, 'test storage failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Exec("DROP TRIGGER IF EXISTS deny_refresh_insert") })
	if _, err := NewAuthService().Refresh(pair.RefreshToken, "test"); err == nil {
		t.Fatal("替换令牌落库失败必须报错")
	}
	if err := common.DB.Exec("DROP TRIGGER deny_refresh_insert").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthService().Refresh(pair.RefreshToken, "retry"); err != nil {
		t.Fatalf("失败的轮换不应烧掉旧令牌，存储恢复后应可重试：%v", err)
	}
}

func TestAuthenticationSnapshotCannotOutlivePasswordChange(t *testing.T) {
	u, _ := lifecycleUser(t)
	var authenticated model.User
	if err := common.DB.First(&authenticated, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewUserService().ChangePassword(u.ID, "old-password", "new-password"); err != nil {
		t.Fatal(err)
	}
	// 模拟另一请求已经完成旧密码校验，但尚未签发令牌的时间窗口。
	if _, err := NewAuthService().issueFor(&authenticated, "late-login"); err == nil {
		t.Fatal("改密之前的认证快照不得在改密之后签发可继续刷新的会话")
	}
}

func TestDisableUserRollsBackWhenTokenRevocationFails(t *testing.T) {
	u, _ := lifecycleUser(t)
	if err := common.DB.Exec("CREATE TRIGGER deny_refresh_revoke BEFORE UPDATE ON refresh_tokens BEGIN SELECT RAISE(ABORT, 'test revoke failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Exec("DROP TRIGGER IF EXISTS deny_refresh_revoke") })
	if err := NewAdminService().SetUserStatus(u.ID+1, u.ID, model.StatusDisabled); err == nil {
		t.Fatal("吊销会话失败时不能报告禁用成功")
	}
	var after model.User
	if err := common.DB.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusEnabled || after.TokenVersion != u.TokenVersion {
		t.Fatalf("禁用失败须整体回滚：status=%s version=%d", after.Status, after.TokenVersion)
	}
}

func TestAdminSettingsUpdateIsAtomic(t *testing.T) {
	setupTestDB(t)
	oldOpen, oldURL := setting.RegistrationOpen(), setting.SiteBaseURL()
	t.Cleanup(func() {
		common.DB.Exec("DROP TRIGGER IF EXISTS deny_site_option")
		_ = setting.SetRegistrationOpen(oldOpen)
		_ = setting.SetSiteBaseURL(oldURL)
	})
	if err := setting.SetRegistrationOpen(false); err != nil {
		t.Fatal(err)
	}
	open, invalidURL := true, "file:///invalid"
	if _, err := NewAdminService().UpdateSettings(UpdateSettingsInput{RegistrationOpen: &open, SiteBaseURL: &invalidURL}); err == nil {
		t.Fatal("无效地址应被拒绝")
	}
	if setting.RegistrationOpen() {
		t.Fatal("后面的字段校验失败不应使前面的开放注册开关生效")
	}
	if err := common.DB.Exec("CREATE TRIGGER deny_site_option BEFORE INSERT ON options WHEN NEW.key = 'site_base_url' BEGIN SELECT RAISE(ABORT, 'test option failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	validURL := "https://example.com"
	if _, err := NewAdminService().UpdateSettings(UpdateSettingsInput{RegistrationOpen: &open, SiteBaseURL: &validURL}); err == nil {
		t.Fatal("配置写入失败应被报告")
	}
	options, err := model.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if setting.RegistrationOpen() || options["registration_open"] != "false" {
		t.Fatal("部分配置持久化失败时数据库和内存都必须保持原值")
	}
}
