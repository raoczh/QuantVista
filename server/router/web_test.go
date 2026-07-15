package router

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func newWebTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	SetWebRouter(r, fstest.MapFS{
		"web/dist/index.html":         {Data: []byte("<html>index-marker</html>")},
		"web/dist/favicon.ico":        {Data: []byte("icon-bytes")},
		"web/dist/assets/app.1a2b.js": {Data: []byte("console.log('hashed')")},
	})
	return r
}

// 缓存头契约：Android 壳"发布即生效"依赖 index.html no-cache 与 /assets/* immutable
// 的组合（壳内没有刷新按钮），改缓存策略前先读 docs/ANDROID_APP_PLAN.md §4.4。
func TestWebCacheHeaders(t *testing.T) {
	r := newWebTestEngine()
	cases := []struct {
		name      string
		path      string
		wantCache string
		wantBody  string
	}{
		{"带 hash 资产长缓存", "/assets/app.1a2b.js", "public, max-age=31536000, immutable", "hashed"},
		{"站点根协商缓存", "/", "no-cache", "index-marker"},
		{"SPA 回退协商缓存", "/stocks/cn/600519", "no-cache", "index-marker"},
		{"无 hash 静态文件协商缓存", "/favicon.ico", "no-cache", "icon-bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != 200 {
				t.Fatalf("%s 状态码 = %d, 期望 200", tc.path, w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != tc.wantCache {
				t.Fatalf("%s Cache-Control = %q, 期望 %q", tc.path, got, tc.wantCache)
			}
			if !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("%s 响应体不含 %q：%s", tc.path, tc.wantBody, w.Body.String())
			}
		})
	}
}

// 目录路径不算静态命中：不能让目录列表拿到 immutable 长缓存，应落 SPA 回退。
func TestWebDirectoryFallsBackToSPA(t *testing.T) {
	r := newWebTestEngine()
	for _, path := range []string{"/assets", "/assets/"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "index-marker") {
			t.Fatalf("%s 应回退 SPA 首页, got code=%d body=%s", path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s Cache-Control = %q, 期望 no-cache", path, got)
		}
	}
}

// /api 未命中保持 JSON 404，不被 SPA 回退吞掉。
func TestWebApiNotFoundStaysJSON(t *testing.T) {
	r := newWebTestEngine()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/not-exist", nil))
	if w.Code != 404 {
		t.Fatalf("/api 未命中状态码 = %d, 期望 404", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/api 未命中应返回 JSON, got Content-Type=%s", w.Header().Get("Content-Type"))
	}
}
