package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestPortfolioCurveHTTPRespectsCanceledRead(t *testing.T) {
	for _, kind := range []string{model.PortfolioKindReal, model.PortfolioKindPaper} {
		t.Run(kind, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 710
			account, err := service.EnsureDefaultPortfolioAccount(userID, kind)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodGet, "/curve?account_id="+strconv.FormatInt(account.ID, 10), nil).WithContext(requestContext)
			if kind == model.PortfolioKindReal {
				NewPositionController(nil).Curve(ctx)
			} else {
				NewPaperController(nil).Curve(ctx)
			}
			var body struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Success {
				t.Error("取消的请求仍继续读取并返回成功")
			}
		})
	}
}

func TestPositionAdviceCanceledRequestNeverStartsJob(t *testing.T) {
	setupPaperControllerReview(t)
	const userID int64 = 711
	account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	const callback = "review_position_advice_canceled_create"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" || tx.Statement.Table == "llm_tasks" {
			attempts++
			tx.AddError(errors.New("本地测试阻止任何后台作业创建"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", userID)
	requestContext, cancel := context.WithCancel(t.Context())
	cancel()
	body := `{"account_id":` + strconv.FormatInt(account.ID, 10) + `}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/positions/advice", strings.NewReader(body)).WithContext(requestContext)
	ctx.Request.Header.Set("Content-Type", "application/json")
	NewPositionAdviceController(service.NewPositionAdviceService(&service.PositionService{}, &service.LLMService{})).Advise(ctx)
	if attempts != 0 {
		t.Fatalf("已取消的持仓建议仍尝试创建 %d 次作业", attempts)
	}
}

func TestPortfolioCurveDoesNotExposeStorageDetails(t *testing.T) {
	setupPaperControllerReview(t)
	account, err := service.EnsureDefaultPortfolioAccount(712, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	const callback = "review_curve_storage_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "portfolio_snapshots" {
			tx.AddError(errors.New("no such table: private_review_ledger"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", int64(712))
	ctx.Request = httptest.NewRequest(http.MethodGet, "/curve?account_id="+strconv.FormatInt(account.ID, 10), nil)
	NewPositionController(nil).Curve(ctx)
	if strings.Contains(response.Body.String(), "private_review_ledger") {
		t.Fatalf("曲线错误暴露内部表名：%s", response.Body.String())
	}
}
