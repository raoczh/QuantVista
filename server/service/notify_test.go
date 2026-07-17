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

// TestNotifyNtfyChannel ntfy 通道：target JSON 校验 + 创建落库 + 更新留空保留 + 老通道零回归。
func TestNotifyNtfyChannel(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM notify_channels")
	common.EncryptionKey = "test-encryption-key-1234567890"
	svc := &NotifyService{}

	// 非法 JSON。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindNtfy, Target: "not-json"}); err == nil {
		t.Fatalf("非法 JSON 应报错")
	}
	// http 拒绝（必须 https）。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindNtfy,
		Target: `{"url":"http://ntfy.example.com","topic":"qv-u1"}`}); err == nil {
		t.Fatalf("http 地址应报错")
	}
	// topic 空。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindNtfy,
		Target: `{"url":"https://ntfy.example.com","topic":""}`}); err == nil {
		t.Fatalf("空 topic 应报错")
	}
	// 空 target 提示 ntfy 专属文案路径（requireTarget=true）。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindNtfy, Target: ""}); err == nil {
		t.Fatalf("空 target 应报错")
	}

	// 合法创建（token 可空）。
	raw := `{"url":"https://ntfy.example.com","topic":"qv-u1","token":"tk_secret"}`
	v, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindNtfy, Target: raw, Enabled: true})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if v.Name != "ntfy 推送" {
		t.Fatalf("默认名应为 ntfy 推送，得到 %s", v.Name)
	}
	if v.TargetCipher != "" || !v.HasTarget {
		t.Fatalf("视图不应含密文且应标记已设 target")
	}
	var rec model.NotifyChannel
	common.DB.First(&rec, v.ID)
	if rec.TargetCipher == raw || rec.TargetCipher == "" {
		t.Fatalf("target 应整串加密落库")
	}
	if dec, _ := common.Decrypt(rec.TargetCipher); dec != raw {
		t.Fatalf("解密应还原整串 JSON，得到 %q", dec)
	}

	// 更新留空 target 保留原密文（ntfy 分支不因空 target 报错）。
	if _, err := svc.Update(1, v.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Name: "手机推送", Enabled: false}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	common.DB.First(&rec, v.ID)
	if dec, _ := common.Decrypt(rec.TargetCipher); dec != raw {
		t.Fatalf("留空 target 应保留原配置")
	}

	// 老通道零回归：serverchan/webhook 校验行为不变。
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: "SCTKEY", Enabled: true}); err != nil {
		t.Fatalf("serverchan 创建应不受影响: %v", err)
	}
	if _, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindWebhook, Target: "https://hook.example.com/x", Enabled: true}); err != nil {
		t.Fatalf("webhook 创建应不受影响: %v", err)
	}
	if !svc.HasEnabledChannel(1) {
		t.Fatalf("应有启用通道")
	}
}

// TestNotifyChannelSwitchKind 换通道类型时必须重填 target（#4）：旧密文是旧类型的地址，
// 留空会被当新类型解析。kind 不变留空保留旧密文的既有语义不动。
func TestNotifyChannelSwitchKind(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM notify_channels")
	common.EncryptionKey = "test-encryption-key-1234567890"
	svc := &NotifyService{}

	v, err := svc.Create(1, NotifyChannelInput{Kind: model.NotifyKindWebhook, Target: "https://hook.example.com/old", Enabled: true})
	if err != nil {
		t.Fatalf("创建 webhook 失败: %v", err)
	}

	// 换 kind 到 ntfy 但 target 留空 → 报错。
	if _, err := svc.Update(1, v.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Enabled: true}); err == nil {
		t.Fatal("切换类型且留空 target 应报错")
	}
	// 报错路径不应改动旧密文。
	var raw model.NotifyChannel
	common.DB.First(&raw, v.ID)
	if raw.Kind != model.NotifyKindWebhook {
		t.Fatalf("报错后 kind 不应变动，得到 %s", raw.Kind)
	}
	if dec, _ := common.Decrypt(raw.TargetCipher); dec != "https://hook.example.com/old" {
		t.Fatalf("报错路径不应改动旧密文，得到 %q", dec)
	}

	// 带新 target 换 kind → 成功且落新密文。
	newTgt := `{"url":"https://ntfy.example.com","topic":"qv-u1"}`
	if _, err := svc.Update(1, v.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Target: newTgt, Enabled: true}); err != nil {
		t.Fatalf("带新 target 切换应成功: %v", err)
	}
	common.DB.First(&raw, v.ID)
	if raw.Kind != model.NotifyKindNtfy {
		t.Fatalf("kind 应更新为 ntfy，得到 %s", raw.Kind)
	}
	if dec, _ := common.Decrypt(raw.TargetCipher); dec != newTgt {
		t.Fatalf("应落新密文，得到 %q", dec)
	}

	// kind 不变留空 target → 保留旧密文（零回归）。
	if _, err := svc.Update(1, v.ID, NotifyChannelInput{Kind: model.NotifyKindNtfy, Name: "手机", Enabled: false}); err != nil {
		t.Fatalf("同 kind 留空更新应成功: %v", err)
	}
	common.DB.First(&raw, v.ID)
	if dec, _ := common.Decrypt(raw.TargetCipher); dec != newTgt {
		t.Fatalf("同 kind 留空应保留密文，得到 %q", dec)
	}
}

// TestParseNtfyTarget 解析规范化：url 尾斜杠去除、字段 trim。
func TestParseNtfyTarget(t *testing.T) {
	tgt, err := parseNtfyTarget(`{"url":" https://ntfy.example.com/ ","topic":" qv-u1 ","token":" tk_x "}`)
	if err != nil {
		t.Fatalf("应解析成功: %v", err)
	}
	if tgt.URL != "https://ntfy.example.com" || tgt.Topic != "qv-u1" || tgt.Token != "tk_x" {
		t.Fatalf("应规范化字段，得到 %+v", tgt)
	}
}

// TestBuildNtfyPayload 发布载荷构造：click 拼接、priority 钳制、kind→tags 映射。
func TestBuildNtfyPayload(t *testing.T) {
	tgt := ntfyTarget{URL: "https://ntfy.example.com", Topic: "qv-u1", Token: "tk_x"}

	// 基本字段 + click 拼接（siteBase 尾斜杠容错）+ tags 映射 + priority 下发。
	p := buildNtfyPayload(tgt, NotifyMessage{
		Title: "标题", Content: "内容", Route: "/alerts", Kind: NotifyMsgKindAlert, Priority: 4,
	}, "https://app.example.com/")
	if p["topic"] != "qv-u1" || p["title"] != "标题" || p["message"] != "内容" {
		t.Fatalf("基本字段不符: %+v", p)
	}
	if p["click"] != "https://app.example.com/alerts" {
		t.Fatalf("click 应为 base+route，得到 %v", p["click"])
	}
	if p["priority"] != 4 {
		t.Fatalf("priority 应下发 4，得到 %v", p["priority"])
	}
	if tags, ok := p["tags"].([]string); !ok || len(tags) != 1 || tags[0] != "bell" {
		t.Fatalf("alert 应映射 tags [bell]，得到 %v", p["tags"])
	}
	// token 绝不进载荷（只走 Authorization 头）。
	if _, ok := p["token"]; ok {
		t.Fatalf("载荷不应含 token")
	}

	// SiteBaseURL 未配置 → 不带 click；Priority 0 → 不带 priority；未知 kind → 不带 tags。
	p = buildNtfyPayload(tgt, NotifyMessage{Title: "t", Content: "c", Route: "/alerts"}, "")
	if _, ok := p["click"]; ok {
		t.Fatalf("siteBase 为空不应带 click")
	}
	if _, ok := p["priority"]; ok {
		t.Fatalf("priority 0 不应下发")
	}
	if _, ok := p["tags"]; ok {
		t.Fatalf("未知 kind 不应带 tags")
	}

	// Route 为空 → 不带 click（即便 siteBase 已配置）。
	p = buildNtfyPayload(tgt, NotifyMessage{Title: "t", Content: "c"}, "https://app.example.com")
	if _, ok := p["click"]; ok {
		t.Fatalf("Route 为空不应带 click")
	}

	// 越界 priority 不下发（ntfy 有效域 1~5）。
	p = buildNtfyPayload(tgt, NotifyMessage{Title: "t", Content: "c", Priority: 9}, "")
	if _, ok := p["priority"]; ok {
		t.Fatalf("越界 priority 不应下发")
	}
}
