package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestPortfolioCanceledRequestsCannotChangeAccountsOrFunds(t *testing.T) {
	for _, action := range []string{"list", "create", "update", "archive", "default", "delete", "cash", "reverse", "targets"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 713
			accounts := service.NewPortfolioAccountService()
			var accountID, flowID int64
			if action != "list" {
				if _, err := accounts.Create(userID, service.PortfolioAccountInput{Name: "默认账户", Kind: "real"}); err != nil {
					t.Fatal(err)
				}
				account, err := accounts.Create(userID, service.PortfolioAccountInput{Name: "保留账户", Kind: "real"})
				if err != nil {
					t.Fatal(err)
				}
				accountID = account.ID
			}
			if action == "reverse" {
				flow, err := service.CreatePortfolioCashFlow(userID, accountID, service.CashFlowInput{Type: "deposit", Amount: 100, TradeDate: "2026-08-01", IdempotencyKey: "before-cancel"})
				if err != nil {
					t.Fatal(err)
				}
				flowID = flow.ID
			}
			var before []model.PortfolioAccount
			if err := common.DB.Where("user_id = ?", userID).Order("id").Find(&before).Error; err != nil {
				t.Fatal(err)
			}
			bodies := map[string]string{
				"create":  `{"name":"不应创建","kind":"real","currency":"CNY"}`,
				"update":  `{"name":"不应改名"}`,
				"cash":    `{"type":"deposit","amount":200,"trade_date":"2026-08-01","idempotency_key":"cancelled-deposit"}`,
				"reverse": `{"idempotency_key":"cancelled-reversal","note":"不应冲正"}`,
				"targets": `{"items":[{"type":"symbol","key":"600901","target_weight_pct":50,"enabled":true}]}`,
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/portfolios/1", strings.NewReader(bodies[action])).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}, {Key: "flow_id", Value: strconv.FormatInt(flowID, 10)}}
			controller := NewPortfolioController(accounts, service.NewPortfolioRiskService(nil, nil))
			handlers := map[string]func(*gin.Context){"list": controller.List, "create": controller.Create, "update": controller.Update, "archive": controller.Archive, "default": controller.Default, "delete": controller.Delete, "cash": controller.CreateCashFlow, "reverse": controller.ReverseCashFlow, "targets": controller.SaveTargets}
			handlers[action](ctx)
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("已取消请求仍返回成功")
			}
			var after []model.PortfolioAccount
			if err := common.DB.Where("user_id = ?", userID).Order("id").Find(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Error("取消请求仍改变了账户数量、名称、默认状态或归档状态")
			}
			var flows, revisions int64
			if err := common.DB.Model(&model.PortfolioCashFlow{}).Where("user_id = ?", userID).Count(&flows).Error; err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Model(&model.TargetAllocationRevision{}).Where("user_id = ?", userID).Count(&revisions).Error; err != nil {
				t.Fatal(err)
			}
			wantFlows := int64(0)
			if action == "reverse" {
				wantFlows = 1
			}
			if flows != wantFlows || revisions != 0 {
				t.Errorf("取消请求仍记录了资金或目标配置：flows=%d want=%d revisions=%d", flows, wantFlows, revisions)
			}
		})
	}
}

func TestPortfolioExplicitRevisionCannotBecomeLatestOrEmpty(t *testing.T) {
	setupPaperControllerReview(t)
	const userID int64 = 714
	account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	risk := service.NewPortfolioRiskService(nil, nil)
	if _, err := risk.SaveTargets(userID, account.ID, []service.TargetAllocationItem{{Type: "symbol", Key: "600901", TargetWeightPct: 50, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	controller := NewPortfolioController(service.NewPortfolioAccountService(), risk)
	for _, revision := range []string{"bad", "-1", "99999999999999999999999", "99"} {
		t.Run(revision, func(t *testing.T) {
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(account.ID, 10)}}
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/portfolios/1/targets?revision="+revision, nil)
			controller.Targets(ctx)
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Errorf("明确请求的无效版本被当成最新版或空配置：%s", response.Body.String())
			}
		})
	}
}

func TestPortfolioErrorHidesStorageInternals(t *testing.T) {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	portfolioError(ctx, errors.New("Error 1146 (42S02): Table 'local_review.private_table' doesn't exist"))
	if strings.Contains(response.Body.String(), "private_table") {
		t.Fatal("组合接口泄露了数据库表名")
	}
}

func TestLegacyImportCanceledRequestCannotCreatePositions(t *testing.T) {
	setupPaperControllerReview(t)
	const userID int64 = 715
	account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "local-cancel.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("symbol,buy_price,buy_date,quantity\n600901,4.0375,2026-08-01,100\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", userID)
	requestContext, cancel := context.WithCancel(t.Context())
	cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/positions/import?account_id=%d", account.ID), &body).WithContext(requestContext)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	NewExportController(service.NewExportService()).ImportPositions(ctx)
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := common.DB.Model(&model.Position{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if result.Success || count != 0 {
		t.Fatalf("取消旧导入仍落账：success=%v count=%d", result.Success, count)
	}
}
