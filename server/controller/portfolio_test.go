package controller

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func callPortfolioUpdate(t *testing.T, controller *PortfolioController, userID, accountID int64) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/api/portfolios/1", strings.NewReader(`{"name":"测试改名"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	c.Set("uid", userID)
	controller.Update(c)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%q", err, w.Body.String())
	}
	return body
}

func TestPortfolioAPICrossUserIDLooksNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:portfolio-controller?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.PortfolioAccount{}); err != nil {
		t.Fatal(err)
	}
	oldDB := common.DB
	common.DB = db
	t.Cleanup(func() { common.DB = oldDB })

	other := model.PortfolioAccount{UserID: 9002, Name: "他人组合", Kind: model.PortfolioKindReal, Currency: "CNY", Status: model.PortfolioStatusActive}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	controller := NewPortfolioController(service.NewPortfolioAccountService(), nil)
	foreign := callPortfolioUpdate(t, controller, 9001, other.ID)
	missing := callPortfolioUpdate(t, controller, 9001, other.ID+9999)
	if foreign["success"] != false || foreign["message"] != "组合不存在" {
		t.Fatalf("跨用户访问应表现为不存在: %+v", foreign)
	}
	if foreign["message"] != missing["message"] || foreign["success"] != missing["success"] {
		t.Fatalf("跨用户与不存在 ID 的响应不可区分: foreign=%+v missing=%+v", foreign, missing)
	}
}
