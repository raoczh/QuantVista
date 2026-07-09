package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// TestEnhanceNewsRoundAutoLLMSwitch 管理后台"自动 LLM 新闻分析"总闸：
// 关闭时即使管理员 LLM 配置可用也不发起任何 LLM 请求，P1 新闻走关键词规则增强；
// 开启时恢复 LLM 调用。开关状态经 setting（options 表）持久化。
func TestEnhanceNewsRoundAutoLLMSwitch(t *testing.T) {
	setupTestDB(t)
	common.EncryptionKey = "unit-test-key"

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"[]"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	// 管理员 + 其默认 LLM 配置（resolveNewsLLM 的两个前提）。
	admin := &model.User{Username: "switch-admin", Role: model.RoleAdmin, Status: model.StatusEnabled}
	if err := common.DB.Create(admin).Error; err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	cipher, _ := common.Encrypt("k")
	cfg := &model.LLMConfig{UserID: admin.ID, Name: "t", Provider: "openai", BaseURL: srv.URL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建 LLM 配置失败: %v", err)
	}

	seedNews := func(id int64, title string) {
		row := &model.News{ID: id, Title: title, Source: "cls", Category: "telegraph",
			PublishTime: time.Now(), SourcePriority: 1, ContentHash: title}
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatalf("seed 新闻失败: %v", err)
		}
	}

	t.Cleanup(func() {
		_ = setting.SetNewsAutoLLM(true) // 恢复默认，防污染共享内存库上的其他测试
		common.DB.Where("1=1").Delete(&model.News{})
		common.DB.Where("1=1").Delete(&model.LLMConfig{})
		common.DB.Delete(admin)
		common.DB.Where("`key` = ?", "news_auto_llm").Delete(&model.Option{})
	})

	// 关闸：不允许自动 LLM → 零请求，规则增强照常落库。
	if err := setting.SetNewsAutoLLM(false); err != nil {
		t.Fatalf("关闭开关失败: %v", err)
	}
	seedNews(9001, "央行宣布降准0.5个百分点释放流动性")
	NewNewsService().EnhanceNewsRound(context.Background())
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("开关关闭时不应有 LLM 请求，实际 %d 次", n)
	}
	var row model.News
	if err := common.DB.First(&row, 9001).Error; err != nil {
		t.Fatalf("取回新闻失败: %v", err)
	}
	if row.Sentiment == "" {
		t.Fatal("开关关闭时新闻应走规则增强（Sentiment 非空），不能留待重试尾巴")
	}

	// 开闸：P1 新闻恢复走 LLM。
	if err := setting.SetNewsAutoLLM(true); err != nil {
		t.Fatalf("开启开关失败: %v", err)
	}
	seedNews(9002, "证监会发布重大资产重组新规征求意见")
	NewNewsService().EnhanceNewsRound(context.Background())
	if n := atomic.LoadInt32(&hits); n == 0 {
		t.Fatal("开关开启且配置可用时，P1 新闻应发起 LLM 请求")
	}
}
