package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"quantvista/model"
	"quantvista/service"
)

func TestPositionExitPreviewHTTPReadOnlyAndOwnership(t *testing.T) {
	db := reviewControllerDB(t, &model.Position{}, &model.PositionTrade{}, &model.PositionExitAssessment{}, &model.PositionExitNotice{},
		&model.DailyBar{}, &model.TradingCalendar{}, &model.Recommendation{}, &model.RecommendationBatch{}, &model.PortfolioAccount{})
	rec := model.Recommendation{ID: 41, UserID: 2, Symbol: "600901", Market: "cn"}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	ctl := NewPositionController(service.NewPositionService(nil))
	cases := []struct {
		body    string
		success bool
	}{
		{`{"symbol":"600901","market":"cn","position_type":"short_term","buy_price":10,"quantity":100}`, true},
		{`{"symbol":"600901","market":"cn","position_type":"short_term","buy_price":10,"quantity":100,"recommendation_id":41}`, false},
		{`{"symbol":"600901","market":"cn","position_type":"short_term","buy_price":10,"quantity":100,"position_id":999}`, false},
		{`{"symbol":"600901","market":"cn","position_type":"short_term","buy_price":10,"quantity":100,"plan_stop_loss":11}`, false},
		{`{"symbol":"600901","market":"cn","position_type":"short_term","buy_price":"NaN","quantity":100}`, false},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("uid", int64(1))
		c.Request = httptest.NewRequest(http.MethodPost, "/api/positions/exit-plan-preview", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		ctl.PreviewExitPlan(c)
		var result struct {
			Success bool                 `json:"success"`
			Data    service.ExitPlanSeed `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Success != tc.success {
			t.Fatalf("预览接口边界错误: %s %v", w.Body.String(), err)
		}
		if result.Success && (result.Data.DataStatus != "unavailable" || result.Data.StopPrice != 0) {
			t.Fatal("没有日线只能报告数据不足，不能返回假规划")
		}
	}
	for _, m := range []any{&model.Position{}, &model.PositionTrade{}, &model.PositionExitAssessment{}, &model.PositionExitNotice{}} {
		var count int64
		if err := db.Model(m).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("预览或失败请求不得创建持仓、流水与提醒")
		}
	}
}

func TestPositionExitRefreshScopesAccountAndKeepsLedger(t *testing.T) {
	db := reviewControllerDB(t, model.AllModels()...)
	accounts := []model.PortfolioAccount{
		{ID: 301, UserID: 1, Name: "本次账户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive},
		{ID: 302, UserID: 1, Name: "另一账户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive},
		{ID: 303, UserID: 2, Name: "另一用户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusActive},
	}
	if err := db.Create(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	positions := []model.Position{}
	for _, a := range accounts {
		positions = append(positions, model.Position{UserID: a.UserID, AccountID: a.ID, Symbol: "600901", Market: "cn", Currency: "CNY", Status: model.PositionStatusHolding, PositionType: model.PositionTypeShortTerm, BuyPrice: 10, Quantity: 100, RemainingCost: 1005})
	}
	if err := db.Create(&positions).Error; err != nil {
		t.Fatal(err)
	}
	ctl := NewPositionController(service.NewPositionService(nil))
	for _, tc := range []struct {
		account string
		success bool
	}{{"301", true}, {"303", false}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("uid", int64(1))
		c.Request = httptest.NewRequest(http.MethodPost, "/api/positions/evaluate-exit?account_id="+tc.account, nil)
		ctl.RefreshExitPlans(c)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Success != tc.success {
			t.Fatalf("刷新账户边界错误: %s %v", w.Body.String(), err)
		}
	}
	var rows []model.PositionExitAssessment
	if err := db.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].UserID != 1 || rows[0].PositionID != positions[0].ID {
		t.Fatalf("只能评估指定本人账户: %+v %v", rows, err)
	}
	var current []model.Position
	if err := db.Order("id").Find(&current).Error; err != nil {
		t.Fatal(err)
	}
	for _, p := range current {
		if p.Quantity != 100 || p.BuyPrice != 10 || p.RemainingCost != 1005 || p.Status != model.PositionStatusHolding {
			t.Fatal("刷新退出评估不能改动交易账本")
		}
	}
}
