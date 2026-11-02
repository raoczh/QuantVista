package common

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RDB Redis 客户端；未配置 REDIS_CONN_STRING 时为 nil（缓存降级为直连数据源）。
var RDB *redis.Client

var ctxBackground = context.Background()

const (
	redisCacheOperationTimeout = 250 * time.Millisecond
	redisCacheFailureCooldown  = 15 * time.Second
)

var errRedisCacheUnavailable = errors.New("Redis cache temporarily unavailable")

// Only cache operations share this breaker. A late success must not reset a
// cooldown opened by a concurrent failure, and a replacement client starts fresh.
var redisCacheCircuit struct {
	sync.Mutex
	client  *redis.Client
	retryAt time.Time
}

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
	// go-redis ignores socket context deadlines unless this option is enabled.
	// Cache failures must not consume seconds before trying the real data source.
	opt.ContextTimeoutEnabled = true
	opt.MaxRetries = -1
	opt.DialTimeout = redisCacheOperationTimeout
	opt.ReadTimeout = redisCacheOperationTimeout
	opt.WriteTimeout = redisCacheOperationTimeout
	opt.PoolTimeout = redisCacheOperationTimeout
	RDB = redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(ctxBackground, 5*time.Second)
	defer cancel()
	if err := RDB.Ping(ctx).Err(); err != nil {
		_ = RDB.Close()
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
	return RedisGetContext(ctxBackground, key)
}

// RedisSet 缓存写入；未启用时静默跳过。
func RedisSet(key, value string, ttl time.Duration) {
	_ = RedisSetContext(ctxBackground, key, value, ttl)
}

// RedisGetContext returns a miss when disabled, canceled, or unavailable. It does
// not introduce a process-wide memory cache; permission/invalidation semantics
// of existing Redis callers therefore remain unchanged.
func RedisGetContext(ctx context.Context, key string) (string, bool) {
	var value string
	found := false
	err := runRedisCacheOperation(ctx, func(operationContext context.Context, client *redis.Client) error {
		var commandError error
		value, commandError = client.Get(operationContext, key).Result()
		found = commandError == nil
		return commandError
	})
	if err != nil || !found {
		return "", false
	}
	return value, true
}

// RedisSetContext uses the caller's earlier deadline or a 250ms cache budget.
// Disabled Redis remains a no-op; other errors are available to interested callers.
func RedisSetContext(ctx context.Context, key, value string, ttl time.Duration) error {
	return runRedisCacheOperation(ctx, func(operationContext context.Context, client *redis.Client) error {
		return client.Set(operationContext, key, value, ttl).Err()
	})
}

// RedisDeleteContext deletes an exact key with the same bounded cache budget.
// This is best-effort infrastructure, not a cross-process publication barrier.
func RedisDeleteContext(ctx context.Context, key string) error {
	return runRedisCacheOperation(ctx, func(operationContext context.Context, client *redis.Client) error {
		return client.Del(operationContext, key).Err()
	})
}

func runRedisCacheOperation(ctx context.Context,
	command func(context.Context, *redis.Client) error) error {
	if ctx == nil {
		ctx = ctxBackground
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client := RDB
	if client == nil {
		return nil
	}
	if !allowRedisCacheOperation(client) {
		return errRedisCacheUnavailable
	}
	operationContext, cancel := context.WithTimeout(ctx, redisCacheOperationTimeout)
	defer cancel()
	err := command(operationContext, client)
	if callerError := ctx.Err(); callerError != nil {
		// A disconnected or impatient caller is not evidence of a Redis outage.
		return callerError
	}
	if operationError := operationContext.Err(); operationError != nil {
		err = operationError
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		recordRedisCacheFailure(client)
	}
	return err
}

func allowRedisCacheOperation(client *redis.Client) bool {
	redisCacheCircuit.Lock()
	defer redisCacheCircuit.Unlock()
	if redisCacheCircuit.client != client {
		redisCacheCircuit.client = client
		redisCacheCircuit.retryAt = time.Time{}
	}
	return !time.Now().Before(redisCacheCircuit.retryAt)
}

func recordRedisCacheFailure(client *redis.Client) {
	redisCacheCircuit.Lock()
	now := time.Now()
	opened := redisCacheCircuit.client == client && !now.Before(redisCacheCircuit.retryAt)
	if opened {
		redisCacheCircuit.retryAt = now.Add(redisCacheFailureCooldown)
	}
	redisCacheCircuit.Unlock()
	if opened {
		SysWarn("Redis 缓存访问失败，暂停缓存请求 %s 后重试", redisCacheFailureCooldown)
	}
}
