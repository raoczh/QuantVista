package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 按 IP 的内存滑窗限流，用于保护登录/首启/OAuth 等敏感端点，防暴力破解。
// 单机内存实现，不依赖 Redis；多实例部署时各实例独立计数（个人自用足够）。

type rlEntry struct {
	count       int
	windowStart time.Time
}

type rateLimiter struct {
	mu        sync.Mutex
	entries   map[string]*rlEntry
	max       int
	window    time.Duration
	lastSweep time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{entries: make(map[string]*rlEntry), max: max, window: window}
}

func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// 懒清理：每过一个窗口扫一次，删除过期条目，防止 map 无界增长。
	if now.Sub(rl.lastSweep) >= rl.window {
		for k, e := range rl.entries {
			if now.Sub(e.windowStart) >= rl.window {
				delete(rl.entries, k)
			}
		}
		rl.lastSweep = now
	}

	e, ok := rl.entries[key]
	if !ok || now.Sub(e.windowStart) >= rl.window {
		rl.entries[key] = &rlEntry{count: 1, windowStart: now}
		return true
	}
	if e.count >= rl.max {
		return false
	}
	e.count++
	return true
}

// RateLimit 返回一个按客户端 IP 限流的中间件：同一 IP 在 window 内最多 max 次。
// 每次调用创建独立计数器，应按端点分别挂载。
func RateLimit(max int, window time.Duration) gin.HandlerFunc {
	rl := newRateLimiter(max, window)
	return func(c *gin.Context) {
		if !rl.allow(c.ClientIP(), time.Now()) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"message": "请求过于频繁，请稍后再试",
			})
			return
		}
		c.Next()
	}
}
