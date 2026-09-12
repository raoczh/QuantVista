package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestMySQLLLMRouteConcurrentUpsertAndReset(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.User{}, &model.LLMConfig{}, &model.LLMModuleRoute{}, &model.LLMCallLog{})
	user := model.User{Username: "route-concurrent-review", Role: model.RoleAdmin}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	cfg := model.LLMConfig{UserID: user.ID, Name: "route-review", Model: "m"}
	if err := db.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func() {
			<-start
			_, err := UpsertLLMRoute(LLMRouteInput{Module: "qa", ConfigID: cfg.ID, Enabled: true})
			results <- err
		}()
	}
	close(start)
	for i := 0; i < 12; i++ {
		if err := <-results; err != nil {
			t.Errorf("同模块并发保存必须均成功：%v", err)
		}
	}
	var rows []model.LLMModuleRoute
	if err := db.Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Revision != 12 {
		t.Fatalf("并发保存必须复用唯一行并保留每次变更：rows=%+v err=%v", rows, err)
	}
	route := rows[0]
	seedRouteCallLogs(t, "qa", cfg.ID, model.LLMCallStatusError, 5, 0)
	if _, ok := evaluateLLMRouteHealth(route, cfg); ok {
		t.Fatal("新失败应触发回退")
	}
	persistRouteFallback(route, "review failure")
	restored, err := ResetLLMRouteFallback(route.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := evaluateLLMRouteHealth(*restored, cfg); !ok {
		t.Fatal("恢复后不得重复使用旧失败")
	}
	persistRouteFallback(route, "review stale failure")
	var stored model.LLMModuleRoute
	if err := common.DB.First(&stored, route.ID).Error; err != nil || stored.AutoFallbackAt != nil {
		t.Fatalf("旧代次故障不能停用已恢复路由：row=%+v err=%v", stored, err)
	}
}
