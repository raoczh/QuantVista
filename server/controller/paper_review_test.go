package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupPaperControllerReview(t *testing.T) (*PaperController, model.PaperHolding) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:paper_reset_review?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	oldDB, oldSQLite := common.DB, common.UsingSQLite
	common.DB, common.UsingSQLite = db, true
	t.Cleanup(func() {
		common.DB, common.UsingSQLite = oldDB, oldSQLite
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatal(err)
	}
	account, err := service.EnsureDefaultPortfolioAccount(701, model.PortfolioKindPaper)
	if err != nil {
		t.Fatal(err)
	}
	holding := model.PaperHolding{UserID: 701, AccountID: account.ID, Symbol: "600000", Market: "cn", Quantity: 100, AvgCost: 10}
	if err := db.Create(&holding).Error; err != nil {
		t.Fatal(err)
	}
	return NewPaperController(service.NewPaperService(nil)), holding
}

func TestPaperResetRejectsMalformedRequestWithoutDeletingFacts(t *testing.T) {
	controller, holding := setupPaperControllerReview(t)
	for _, body := range []string{`{"initial_cash":"录入错误"}`, `{"initial_cash":50000,`} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Set("uid", int64(701))
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/paper/reset", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		controller.Reset(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Success {
			t.Errorf("错误请求不得成功重置：%s", body)
		}
		var count int64
		if err := common.DB.Model(&model.PaperHolding{}).Where("id = ?", holding.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatal("请求解析失败却清空了模拟持仓")
		}
	}
}

func TestPaperResetCanceledRequestKeepsFacts(t *testing.T) {
	controller, holding := setupPaperControllerReview(t)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", int64(701))
	reqCtx, cancel := context.WithCancel(t.Context())
	cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/paper/reset", strings.NewReader(`{"initial_cash":50000}`)).WithContext(reqCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	controller.Reset(ctx)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("已取消的重置请求仍成功返回")
	}
	var count int64
	if err := common.DB.Model(&model.PaperHolding{}).Where("id = ?", holding.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("已取消请求删除了模拟持仓")
	}
}
