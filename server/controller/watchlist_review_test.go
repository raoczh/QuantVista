package controller

import (
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

func TestWatchlistPatchDoesNotEraseUnsubmittedFields(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:watchlist_patch_review?mode=memory&cache=shared"), &gorm.Config{})
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
	if err := db.AutoMigrate(&model.Watchlist{}, &model.WatchlistItem{}); err != nil {
		t.Fatal(err)
	}
	group := model.Watchlist{UserID: 702, Name: "排序应保留", SortOrder: 7}
	if err := db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	item := model.WatchlistItem{UserID: 702, WatchlistID: group.ID, Symbol: "600103", Market: "cn", Note: "已有备注", FocusReason: "已有关注原因"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	controller := NewWatchlistController(service.NewWatchlistService(nil))
	for _, operation := range []struct {
		body string
		run  func(*gin.Context)
	}{
		{`{"is_pinned":true}`, controller.UpdateItem},
		{`{"name":"新名称"}`, controller.UpdateGroup},
	} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Set("uid", int64(702))
		ctx.Params = gin.Params{{Key: "id", Value: "1"}}
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/watchlist-review/1", strings.NewReader(operation.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		operation.run(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success {
			t.Fatalf("修改失败：%s", response.Body.String())
		}
	}
	if err := db.First(&item, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !item.IsPinned || item.Note != "已有备注" || item.FocusReason != "已有关注原因" {
		t.Errorf("切换重点覆盖了未提交字段：%+v", item)
	}
	if err := db.First(&group, group.ID).Error; err != nil {
		t.Fatal(err)
	}
	if group.Name != "新名称" || group.SortOrder != 7 {
		t.Errorf("重命名不应清零排序：%+v", group)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", int64(702))
	ctx.Params = gin.Params{{Key: "id", Value: "1"}}
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/watchlist-review/1", strings.NewReader(`{"is_pinned":false,"note":""}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	controller.UpdateItem(ctx)
	if err := db.First(&item, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if item.IsPinned || item.Note != "" || item.FocusReason != "已有关注原因" {
		t.Errorf("显式 false/空备注必须可保存，同时保留未提交的关注原因：%+v", item)
	}
}
