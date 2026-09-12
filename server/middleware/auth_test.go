package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestJWTAuthUsesCurrentRoleAndPreservesSessionOnDatabaseFailure(t *testing.T) {
	previous := common.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	connection.SetMaxOpenConns(1)
	common.DB = db
	t.Cleanup(func() { common.DB = previous; connection.Close() })
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{ID: 1, Username: "role-check", Role: model.RoleUser, Status: model.StatusEnabled}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	token, _, err := common.IssueAccessToken(1, model.RoleAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/admin", JWTAuth(), AdminAuth(), func(c *gin.Context) { c.Status(http.StatusOK) })
	request := func() int {
		req := httptest.NewRequest(http.MethodGet, "/admin", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response.Code
	}
	if status := request(); status != http.StatusForbidden {
		t.Fatalf("旧 token 的 admin 不能覆盖数据库当前角色：%d", status)
	}
	if err := db.Model(&user).Update("role", model.RoleAdmin).Error; err != nil {
		t.Fatal(err)
	}
	if status := request(); status != http.StatusOK {
		t.Fatalf("当前管理员应有访问权限：%d", status)
	}
	callback := "test:auth_database_unavailable"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		tx.AddError(errors.New("database unavailable"))
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callback)
	if status := request(); status != http.StatusServiceUnavailable {
		t.Fatalf("数据库故障应为 503，不能误报凭证失效导致全站退出：%d", status)
	}
}
