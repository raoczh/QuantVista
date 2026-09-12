package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestLLMHTTPHonorsCancellation(t *testing.T) {
	for _, action := range []string{"create", "update", "delete", "default", "list", "probe", "models"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			oldKey := common.EncryptionKey
			common.EncryptionKey = "local-review"
			t.Cleanup(func() { common.EncryptionKey = oldKey })
			user := model.User{Username: "llm-cancel", Role: model.RoleAdmin}
			if err := common.DB.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			cipher, err := common.Encrypt("local-fixture")
			if err != nil {
				t.Fatal(err)
			}
			cfg := model.LLMConfig{UserID: user.ID, Name: "旧配置", BaseURL: "https://local.example", Model: "m", APIKeyCipher: cipher}
			if err := common.DB.Create(&cfg).Error; err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer srv.Close()
			in := service.LLMConfigInput{Name: "新配置", BaseURL: srv.URL, Model: "m", APIKey: "local-fixture", MaxTokens: 100, IsDefault: true}
			body, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			c.Set("uid", user.ID)
			c.Set("role", model.RoleAdmin)
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(cfg.ID, 10)}}
			c.Request = httptest.NewRequest(http.MethodPost, "/api/llm-configs", strings.NewReader(string(body))).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			ctl := NewLLMController(service.NewLLMService())
			switch action {
			case "create":
				ctl.Create(c)
			case "update":
				ctl.Update(c)
			case "delete":
				ctl.Delete(c)
			case "default":
				ctl.SetDefault(c)
			case "list":
				ctl.List(c)
			case "probe":
				ctl.TestDraft(c)
			case "models":
				ctl.FetchModels(c)
			}
			var reply struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil || reply.Success {
				t.Errorf("取消请求不能报告成功：action=%s body=%s err=%v", action, recorder.Body.String(), err)
			}
			if calls.Load() != 0 {
				t.Errorf("已取消请求仍调用模型接口：%d", calls.Load())
			}
			var rows []model.LLMConfig
			if err := common.DB.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Name != "旧配置" || rows[0].IsDefault {
				t.Fatalf("取消请求改变了配置：rows=%+v err=%v", rows, err)
			}
		})
	}
}
