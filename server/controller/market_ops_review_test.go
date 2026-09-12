package controller

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMarketOpsCanceledPauseKeepsJob(t *testing.T) {
	for _, route := range []string{"wide_pause", "task_cancel"} {
		t.Run(route, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ops.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&model.User{}, &model.JobRun{}, &model.JobStep{}, &model.JobEvent{}); err != nil {
				t.Fatal(err)
			}
			old := common.DB
			common.DB = db
			sqlDB, _ := db.DB()
			t.Cleanup(func() { common.DB = old; _ = sqlDB.Close() })
			actor := model.User{Username: "local-ops", Role: model.RoleAdmin, Status: model.StatusEnabled}
			if err := db.Create(&actor).Error; err != nil {
				t.Fatal(err)
			}
			run := model.JobRun{OwnerType: model.JobOwnerSystem, Kind: service.JobKindInitMarketHistory, Status: model.JobStatusQueued}
			if err := db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/api/admin/market/wide-init/pause", nil).WithContext(ctx)
			c.Set("uid", actor.ID)
			c.Set("role", model.RoleAdmin)
			c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(run.ID, 10)}}
			if route == "wide_pause" {
				(&MarketController{}).WideInitPause(c)
			} else {
				NewTaskCenterController(&service.TaskCenterService{}).Cancel(c)
			}
			var after model.JobRun
			if err := db.First(&after, run.ID).Error; err != nil {
				t.Fatal(err)
			}
			var result struct{ Success bool }
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Success || after.Status != model.JobStatusQueued || after.CancelRequested {
				t.Fatalf("取消的请求仍终止了后台任务: status=%s cancel=%v response=%s", after.Status, after.CancelRequested, w.Body.String())
			}
		})
	}
}
