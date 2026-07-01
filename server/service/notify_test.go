package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestNotifyChannelCRUD 通道 CRUD + 密文不外泄 + 校验 + 隔离。
func TestNotifyChannelCRUD(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM notify_channels")
	// 加密依赖 ENCRYPTION_KEY；测试里直接设置包变量。
	common.EncryptionKey = "test-encryption-key-1234567890"
	svc := &NotifyService{}

	// 非法类型。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: "sms", Target: "x"}); err == nil {
		t.Fatalf("非法类型应报错")
	}
	// webhook 非法地址。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindWebhook, Target: "ftp://x"}); err == nil {
		t.Fatalf("非法 webhook 地址应报错")
	}
	// 空 target。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: ""}); err == nil {
		t.Fatalf("空 target 应报错")
	}

	// 正常创建 Server酱。
	v, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCT123KEY", Enabled: true})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if v.TargetCipher != "" {
		t.Fatalf("视图不应含密文")
	}
	if !v.HasTarget {
		t.Fatalf("应标记已设 target")
	}
	if v.Name != "Server酱" {
		t.Fatalf("默认名应为 Server酱，得到 %s", v.Name)
	}

	// 密文确实加密落库（非明文）。
	var raw model.NotifyChannel
	common.DB.First(&raw, v.ID)
	if raw.TargetCipher == "" || raw.TargetCipher == "SCT123KEY" {
		t.Fatalf("target 应加密落库，得到 %q", raw.TargetCipher)
	}
	dec, _ := common.Decrypt(raw.TargetCipher)
	if dec != "SCT123KEY" {
		t.Fatalf("解密应还原，得到 %q", dec)
	}

	// HasEnabledChannel。
	if !svc.HasEnabledChannel(1) {
		t.Fatalf("应有启用通道")
	}
	if svc.HasEnabledChannel(2) {
		t.Fatalf("用户2 不应有通道")
	}

	// 更新留空 target 保留原密文。
	if _, err := svc.Update(1, v.ID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Name: "我的Server酱", Enabled: false}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	common.DB.First(&raw, v.ID)
	if dec, _ := common.Decrypt(raw.TargetCipher); dec != "SCT123KEY" {
		t.Fatalf("留空 target 应保留原密钥")
	}
	if raw.Enabled {
		t.Fatalf("应已禁用")
	}

	// 跨用户 Update/Delete 隔离。
	if _, err := svc.Update(2, v.ID, NotifyChannelInput{Kind: model.NotifyKindServerChan}); err == nil {
		t.Fatalf("跨用户 Update 应失败")
	}
	if err := svc.Delete(2, v.ID); err == nil {
		t.Fatalf("跨用户 Delete 应失败")
	}

	// List 不含密文。
	list, _ := svc.List(1)
	if len(list) != 1 || list[0].TargetCipher != "" {
		t.Fatalf("列表应 1 条且不含密文")
	}

	// 本人删除。
	if err := svc.Delete(1, v.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
}
