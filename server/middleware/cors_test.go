package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

func TestCORSAllowsAuthenticatedEventStreamResume(t *testing.T) {
	previous, origins := common.Production, common.AllowedOrigins
	common.Production, common.AllowedOrigins = true, []string{"https://review.example"}
	t.Cleanup(func() { common.Production, common.AllowedOrigins = previous, origins })
	router := gin.New()
	router.Use(CORS())
	req := httptest.NewRequest(http.MethodOptions, "/api/tasks/events", nil)
	req.Header.Set("Origin", "https://review.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization,last-event-id")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), "last-event-id") {
		t.Fatalf("事件续传预检必须允许 Last-Event-ID：status=%d headers=%v", w.Code, w.Header())
	}
}
