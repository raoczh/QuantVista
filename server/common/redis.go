package common

import (
	"context"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// RDB Redis 客户端；未配置 REDIS_CONN_STRING 时为 nil（缓存降级为直连数据源）。
var RDB *redis.Client

var ctxBackground = context.Background()

// InitRedis 可选初始化。REDIS_CONN_STRING 为空时跳过，不报错。
func InitRedis() error {
	conn := os.Getenv("REDIS_CONN_STRING")
	if conn == "" {
		SysLog("REDIS_CONN_STRING 未设置，Redis 缓存关闭")
		return nil
	}
	opt, err := redis.ParseURL(conn)
	if err != nil {
		return err
	}
	RDB = redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(ctxBackground, 5*time.Second)
	defer cancel()
	if err := RDB.Ping(ctx).Err(); err != nil {
		RDB = nil
		return err
	}
	SysLog("Redis 已连接")
	return nil
}

// RedisEnabled 是否启用了 Redis。
func RedisEnabled() bool { return RDB != nil }

// RedisGet 缓存读取；未启用或 miss 返回 ("", false)。
func RedisGet(key string) (string, bool) {
	if RDB == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctxBackground, 2*time.Second)
	defer cancel()
	v, err := RDB.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

// RedisSet 缓存写入；未启用时静默跳过。
func RedisSet(key, value string, ttl time.Duration) {
	if RDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctxBackground, 2*time.Second)
	defer cancel()
	_ = RDB.Set(ctx, key, value, ttl).Err()
}
