package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestLLMConfigCreatePreservesExplicitZeroAndFalse(t *testing.T) {
	setupTestDB(t)
	user := model.User{Username: "llm-zero-review"}
	if err := common.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-zero-review-key"
	t.Cleanup(func() { common.EncryptionKey = oldKey })
	in := validLLMConfigInput()
	in.APIKey, in.Temperature, in.Stream = "review-key", 0, false
	created, err := NewLLMService().Create(user.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	var stored model.LLMConfig
	if err := common.DB.First(&stored, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if created.Temperature != 0 || created.Stream || stored.Temperature != 0 || stored.Stream {
		t.Fatalf("显式温度 0 和关闭流式必须原样保存：返回=%v/%v，存储=%v/%v",
			created.Temperature, created.Stream, stored.Temperature, stored.Stream)
	}
}
