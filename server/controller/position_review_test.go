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

func TestPositionCanceledActionsKeepLedger(t *testing.T) {
	for _, action := range []string{"update", "buy", "close", "delete", "link", "trades"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 704
			account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
			if err != nil {
				t.Fatal(err)
			}
			p := model.Position{UserID: userID, AccountID: account.ID, Symbol: "600901", Market: "cn", Currency: "CNY",
				PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
				BuyPrice: 10, Quantity: 100, BuyDate: "2026-08-01", TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000,
				PeakPrice: 10, PeakFrom: "2026-08-01", PeakDate: "2026-08-01", RecommendationID: 91}
			if action == "trades" {
				p.TotalBuyCost, p.TotalBuyQty, p.RemainingCost = 0, 0, 0
			}
			if err := common.DB.Create(&p).Error; err != nil {
				t.Fatal(err)
			}
			if action != "trades" {
				if err := common.DB.Create(&model.PositionTrade{UserID: userID, AccountID: account.ID, PositionID: p.ID,
					Side: "buy", Price: 10, Quantity: 100, TradeDate: p.BuyDate, QuantityAfter: 100, AvgCostAfter: 10}).Error; err != nil {
					t.Fatal(err)
				}
			}
			bodies := map[string]string{
				"update": `{"buy_price":10,"quantity":100,"buy_date":"2026-08-01","position_type":"long_term","user_note":"不应写入"}`,
				"buy":    `{"side":"buy","price":10,"quantity":100,"trade_date":"2026-08-02"}`,
				"close":  `{"sell_price":11,"sell_date":"2026-08-02"}`,
				"link":   `{"recommendation_id":0}`,
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/positions/"+strconv.FormatInt(p.ID, 10)+"?account_id="+strconv.FormatInt(account.ID, 10), strings.NewReader(bodies[action])).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(p.ID, 10)}}
			controller := NewPositionController(service.NewPositionService(nil))
			handlers := map[string]func(*gin.Context){"update": controller.Update, "buy": controller.AddTrade, "close": controller.Close,
				"delete": controller.Delete, "link": controller.LinkRecommendation, "trades": controller.Trades}
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
			var current model.Position
			if err := common.DB.First(&current, p.ID).Error; err != nil {
				t.Fatalf("取消请求不应删除持仓：%v", err)
			}
			if current.Quantity != p.Quantity || current.Status != p.Status || current.UserNote != p.UserNote ||
				current.RecommendationID != p.RecommendationID || current.TotalBuyCost != p.TotalBuyCost {
				t.Errorf("取消请求仍改变了账本：%+v", current)
			}
			var trades int64
			if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&trades).Error; err != nil {
				t.Fatal(err)
			}
			wantTrades := int64(1)
			if action == "trades" {
				wantTrades = 0
			}
			if trades != wantTrades {
				t.Errorf("已取消请求仍增删流水：got=%d want=%d", trades, wantTrades)
			}
		})
	}
}

func seedCorpAdjustControllerReview(t *testing.T) (*PositionController, model.Position, model.PositionCorpAdjust) {
	t.Helper()
	setupPaperControllerReview(t)
	const userID int64 = 705
	account, err := service.EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Position{UserID: userID, AccountID: account.ID, Symbol: "600901", Market: "cn", Currency: "CNY",
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: 10, Quantity: 100, BuyDate: "2026-08-01", TotalBuyCost: 1000, TotalBuyQty: 100, RemainingCost: 1000}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.PositionTrade{UserID: userID, AccountID: account.ID, PositionID: p.ID,
		Side: "buy", Price: 10, Quantity: 100, TradeDate: p.BuyDate, QuantityAfter: 100, AvgCostAfter: 10}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.CorporateAction{Symbol: p.Symbol, Market: p.Market, ReportDate: "2026-06-30",
		ExDate: "2026-08-04", RecordDate: "2026-08-03", TransferRatio: 10, Progress: model.CorpActionProgressImplemented}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.GenerateCorpAdjustsForAccount(userID, account.ID, "2026-08-05"); err != nil {
		t.Fatal(err)
	}
	rows, err := service.ListCorpAdjustsForAccount(userID, account.ID, "pending")
	if err != nil || len(rows) != 1 {
		t.Fatalf("构造待确认建议：rows=%+v err=%v", rows, err)
	}
	return NewPositionController(service.NewPositionService(nil)), p, rows[0]
}

func TestCorpAdjustCanceledActionsKeepLedger(t *testing.T) {
	for _, action := range []string{"confirm", "dismiss", "revert", "generate"} {
		t.Run(action, func(t *testing.T) {
			controller, p, adj := seedCorpAdjustControllerReview(t)
			if action == "revert" {
				confirmed, err := controller.svc.ConfirmCorpAdjustForAccount(p.UserID, p.AccountID, adj.ID)
				if err != nil {
					t.Fatal(err)
				}
				adj = *confirmed
				if err := common.DB.First(&p, p.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			var beforeCount int64
			if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&beforeCount).Error; err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", p.UserID)
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			body, _ := json.Marshal(map[string]string{"context_version": adj.ContextVersion})
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/positions/corp-adjusts?account_id="+strconv.FormatInt(p.AccountID, 10), strings.NewReader(string(body))).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(adj.ID, 10)}, {Key: "action", Value: action}}
			if action == "generate" {
				controller.CorpAdjusts(ctx)
			} else {
				controller.CorpAdjustAction(ctx)
			}
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("取消的折算请求仍返回成功")
			}
			var current model.Position
			if err := common.DB.First(&current, p.ID).Error; err != nil || current.Quantity != p.Quantity || current.RealizedPnl != p.RealizedPnl {
				t.Fatalf("取消后仍改动持仓：current=%+v err=%v", current, err)
			}
			var currentAdjust model.PositionCorpAdjust
			if err := common.DB.First(&currentAdjust, adj.ID).Error; err != nil || currentAdjust.Status != adj.Status || currentAdjust.TradeID != adj.TradeID {
				t.Fatalf("取消后仍改动折算状态：current=%+v err=%v", currentAdjust, err)
			}
			var afterCount int64
			if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Count(&afterCount).Error; err != nil || afterCount != beforeCount {
				t.Fatalf("取消后仍增删折算流水：count=%d err=%v", afterCount, err)
			}
		})
	}
}

func TestCorpAdjustGenerationFailureIsNotSuccessfulEmptyRead(t *testing.T) {
	controller, p, _ := seedCorpAdjustControllerReview(t)
	const callback = "review_corp_adjust_generation_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "corporate_actions" {
			tx.AddError(errors.New("本机来源查询失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", p.UserID)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/positions/corp-adjusts?account_id="+strconv.FormatInt(p.AccountID, 10), nil)
	controller.CorpAdjusts(ctx)
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success || !strings.Contains(result.Message, "来源查询失败") {
		t.Fatalf("来源读取失败必须作为失败交付，不能冒充已刷新：%+v", result)
	}
}
