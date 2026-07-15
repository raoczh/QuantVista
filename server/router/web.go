package router

import (
	"io/fs"
	"net/http"
	"strings"

	"quantvista/common"

	"github.com/gin-gonic/gin"
)

// SetWebRouter 托管 embed 进二进制的 Vue 构建产物（SPA）。
// 非 /api 且非静态资源的路径一律回退 index.html，交给前端路由。
//
// 缓存策略（Android 壳"发布即生效"依赖这两条，壳内没有刷新按钮）：
//   - index.html 恒 no-cache：每次协商，Docker 发新版后冷启动即见新页面；
//   - /assets/* 是 vite 带内容 hash 的产物，immutable 长缓存，弱网/壳内二次打开直接走本地缓存。
func SetWebRouter(r *gin.Engine, webFS fs.FS) {
	// 去掉 embed 的 "web/dist" 前缀，得到以站点根为基准的子文件系统。
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		common.SysWarn("前端资源未就绪（embed web/dist 失败）：%v", err)
		return
	}

	indexBytes, idxErr := fs.ReadFile(sub, "index.html")
	fileServer := http.FileServer(http.FS(sub))

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path

		// /api 未命中应返回 JSON 404，而不是前端页面。
		if strings.HasPrefix(path, "/api") {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "接口不存在"})
			return
		}

		// 命中真实静态文件则直接返回（目录不算命中，落 SPA 回退，防目录列表拿到长缓存头）。
		trimmed := strings.TrimPrefix(path, "/")
		if trimmed != "" {
			if f, err := sub.Open(trimmed); err == nil {
				st, statErr := f.Stat()
				_ = f.Close()
				if statErr == nil && !st.IsDir() {
					if strings.HasPrefix(path, "/assets/") {
						c.Header("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						// index.html 直访及 favicon 等无 hash 文件：协商缓存，保证发布即生效。
						c.Header("Cache-Control", "no-cache")
					}
					fileServer.ServeHTTP(c.Writer, c.Request)
					return
				}
			}
		}

		// 其余回退 SPA 首页。
		if idxErr != nil {
			c.String(http.StatusOK, "QuantVista 前端尚未构建（占位）。开发期请用 vite dev server。")
			return
		}
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexBytes)
	})
}
