package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestWatchlistCanceledRequestsKeepUserFacts(t *testing.T) {
	for _, action := range []string{"list", "create_group", "rename_group", "delete_group", "update_item", "delete_item", "stage"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 720
			group := model.Watchlist{UserID: userID, Name: "原始分组"}
			if action != "list" {
				if err := common.DB.Create(&group).Error; err != nil {
					t.Fatal(err)
				}
			}
			item := model.WatchlistItem{UserID: userID, WatchlistID: group.ID, Symbol: "600001", Market: "cn", Note: "原始备注"}
			if action != "list" {
				if err := common.DB.Create(&item).Error; err != nil {
					t.Fatal(err)
				}
			}
			controller := NewWatchlistController(service.NewWatchlistService(service.NewMarketService(datasource.NewManagerWithAdapters())))
			handlers := map[string]func(*gin.Context){"list": controller.List, "create_group": controller.CreateGroup, "rename_group": controller.UpdateGroup,
				"delete_group": controller.DeleteGroup, "update_item": controller.UpdateItem, "delete_item": controller.DeleteItem, "stage": controller.SetItemStage}
			bodies := map[string]string{"create_group": `{"name":"取消后新建"}`, "rename_group": `{"name":"取消后改名"}`, "update_item": `{"note":"取消后备注"}`, "stage": `{"stage":"planned"}`}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			id := group.ID
			if action == "update_item" || action == "delete_item" || action == "stage" {
				id = item.ID
			}
			ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, "/local-watchlist-review", strings.NewReader(bodies[action])).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			handlers[action](ctx)
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success {
				t.Error("取消后仍返回操作成功")
			}
			var groups []model.Watchlist
			if err := common.DB.Where("user_id = ?", userID).Find(&groups).Error; err != nil {
				t.Fatal(err)
			}
			if action == "list" {
				if len(groups) != 0 {
					t.Errorf("已取消列表读取创建了默认分组：%+v", groups)
				}
			} else {
				if len(groups) != 1 || groups[0].Name != "原始分组" {
					t.Errorf("取消后分组改变：%+v", groups)
				}
				var items []model.WatchlistItem
				if err := common.DB.Where("user_id = ?", userID).Find(&items).Error; err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Note != "原始备注" || items[0].ResearchStage != "" {
					t.Errorf("取消后条目改变：%+v", items)
				}
			}
		})
	}
}

func TestWatchlistBatchHTTPRespectsCancellation(t *testing.T) {
	for _, action := range []string{"create", "get", "undo"} {
		t.Run(action, func(t *testing.T) {
			setupPaperControllerReview(t)
			const userID int64 = 721
			group := model.Watchlist{UserID: userID, Name: "批量取消审查"}
			if err := common.DB.Create(&group).Error; err != nil {
				t.Fatal(err)
			}
			hash := func(raw string) string { sum := sha256.Sum256([]byte(raw)); return hex.EncodeToString(sum[:]) }
			request, result := `{}`, `{"items":[{"symbol":"600001","name":"本地股票"}]}`
			scan := model.StrategyRunResult{UserID: userID, Kind: service.JobKindScreenerScan, JobRunID: 100,
				RequestJSON: request, RequestHash: hash(request), ResultJSON: result, ContentHash: hash(result), Status: model.JobStatusSuccess}
			if err := common.DB.Create(&scan).Error; err != nil {
				t.Fatal(err)
			}
			id := strconv.FormatInt(scan.ID, 10)
			if action != "create" {
				batch, err := service.CreateWatchlistBatch(userID, scan.ID, service.WatchlistBatchRequest{GroupID: group.ID, Symbols: []string{"600001"}})
				if err != nil {
					t.Fatal(err)
				}
				id = batch.ID
			}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Set("uid", userID)
			ctx.Params = gin.Params{{Key: "id", Value: id}}
			requestContext, cancel := context.WithCancel(t.Context())
			cancel()
			body := `{"group_id":` + strconv.FormatInt(group.ID, 10) + `,"symbols":["600001"]}`
			ctx.Request = httptest.NewRequest(http.MethodPost, "/local-batch-review", strings.NewReader(body)).WithContext(requestContext)
			ctx.Request.Header.Set("Content-Type", "application/json")
			controller := NewWatchlistBatchController()
			map[string]func(*gin.Context){"create": controller.Create, "get": controller.Get, "undo": controller.Undo}[action](ctx)
			var reply struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Success {
				t.Error("取消的批量请求仍返回成功")
			}
			var items int64
			if err := common.DB.Model(&model.WatchlistItem{}).Where("user_id = ?", userID).Count(&items).Error; err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if action == "create" {
				want = 0
			}
			if items != want {
				t.Errorf("取消的批量请求增删了条目：got=%d want=%d", items, want)
			}
		})
	}
}

func TestWatchlistStorageErrorDoesNotExposeTable(t *testing.T) {
	setupPaperControllerReview(t)
	const callback = "review_watchlist_storage_error"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "watchlists" {
			tx.AddError(errors.New("no such table: private_watch_records"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("uid", int64(722))
	ctx.Request = httptest.NewRequest(http.MethodGet, "/watchlists", nil)
	NewWatchlistController(service.NewWatchlistService(nil)).List(ctx)
	if strings.Contains(response.Body.String(), "private_watch_records") {
		t.Fatalf("自选接口暴露内部表名：%s", response.Body.String())
	}
}
