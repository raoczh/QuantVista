package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestDataImportCanceledRequestsKeepBatchAndLedger(t *testing.T) {
	for _, action := range []string{"upload", "preview", "confirm", "rollback"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 711
			svc := service.NewDataImportService()
			account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
			if err != nil {
				t.Fatal(err)
			}
			csv := "symbol,price,quantity,trade_date\n600901,4.0375,100,2026-08-01\n"
			var batch *service.ImportBatchView
			if action != "upload" {
				batch, err = svc.UploadByAccount(userID, account.ID, "position", "cancel.csv", strings.NewReader(csv))
				if err != nil {
					t.Fatal(err)
				}
				if action != "preview" {
					batch, err = svc.Preview(userID, batch.ID, service.ImportMappingInput{Version: batch.Version, Mapping: batch.Suggestions})
					if err != nil {
						t.Fatal(err)
					}
				}
				if action == "rollback" {
					batch, err = svc.Confirm(t.Context(), userID, batch.ID, service.ImportConfirmInput{Version: batch.Version})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			var body bytes.Buffer
			contentType := "application/json"
			if action == "upload" {
				writer := multipart.NewWriter(&body)
				if err := writer.WriteField("kind", "position"); err != nil {
					t.Fatal(err)
				}
				file, err := writer.CreateFormFile("file", "cancel.csv")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte(csv)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				contentType = writer.FormDataContentType()
			} else if action == "preview" {
				if err := json.NewEncoder(&body).Encode(service.ImportMappingInput{Version: batch.Version, Mapping: batch.Suggestions}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := json.NewEncoder(&body).Encode(service.ImportConfirmInput{Version: batch.Version}); err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/imports?account_id=%d", account.ID), &body).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", contentType)
			if batch != nil {
				ctx.Params = gin.Params{{Key: "id", Value: batch.ID}}
			}
			controller := NewDataImportController(svc)
			handlers := map[string]func(*gin.Context){"upload": controller.Upload, "preview": controller.Preview, "confirm": controller.Confirm, "rollback": controller.Rollback}
			handlers[action](ctx)
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("已取消的导入请求仍返回成功")
			}
			var stored []model.ImportBatch
			if err := common.DB.Where("user_id = ?", userID).Find(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if action == "upload" {
				if len(stored) != 0 {
					t.Errorf("取消上传仍创建了 %d 个批次", len(stored))
				}
			} else if len(stored) != 1 || stored[0].Status != batch.Status || stored[0].Version != batch.Version {
				t.Errorf("取消请求仍改变了批次：%+v", stored)
			}
			want := int64(0)
			if action == "rollback" {
				want = 1
			}
			for _, table := range []any{&model.Position{}, &model.PositionTrade{}} {
				var count int64
				if err := common.DB.Model(table).Where("user_id = ?", userID).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Errorf("取消请求仍增删了账本：%T count=%d want=%d", table, count, want)
				}
			}
		})
	}
}
