package common

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisContextTestHook struct {
	process func(context.Context, redis.Cmder) error
}

func (hook redisContextTestHook) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("network access is forbidden in Redis context tests")
	}
}

func (hook redisContextTestHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return hook.process
}

func (hook redisContextTestHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		for _, command := range commands {
			if err := hook.process(ctx, command); err != nil {
				return err
			}
		}
		return nil
	}
}

func installRedisContextTestClient(test *testing.T, process func(context.Context, redis.Cmder) error) *redis.Client {
	test.Helper()
	previousClient := RDB
	redisCacheCircuit.Lock()
	previousCircuitClient, previousRetryAt := redisCacheCircuit.client, redisCacheCircuit.retryAt
	redisCacheCircuit.client, redisCacheCircuit.retryAt = nil, time.Time{}
	redisCacheCircuit.Unlock()
	client := redis.NewClient(&redis.Options{
		Addr:                  "unused.invalid:6379",
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	})
	client.AddHook(redisContextTestHook{process: process})
	RDB = client
	test.Cleanup(func() {
		_ = client.Close()
		RDB = previousClient
		redisCacheCircuit.Lock()
		redisCacheCircuit.client, redisCacheCircuit.retryAt = previousCircuitClient, previousRetryAt
		redisCacheCircuit.Unlock()
	})
	return client
}

func TestRedisContextDisabledDoesNotCreateMemoryCache(test *testing.T) {
	previousClient := RDB
	RDB = nil
	defer func() { RDB = previousClient }()
	RedisSet("permission:test", "allowed", time.Minute)
	if value, found := RedisGet("permission:test"); found || value != "" {
		test.Fatalf("disabled Redis unexpectedly retained data: %q, %v", value, found)
	}
	if err := RedisSetContext(nil, "key", "value", time.Minute); err != nil {
		test.Fatal(err)
	}
	if err := RedisDeleteContext(nil, "key"); err != nil {
		test.Fatal(err)
	}
	if value, found := RedisGetContext(nil, "key"); found || value != "" {
		test.Fatalf("disabled context-aware Redis should miss: %q, %v", value, found)
	}
}

func TestRedisContextBudgetAndFailureCircuit(test *testing.T) {
	var calls atomic.Int32
	installRedisContextTestClient(test, func(ctx context.Context, _ redis.Cmder) error {
		calls.Add(1)
		deadline, hasDeadline := ctx.Deadline()
		if remaining := time.Until(deadline); !hasDeadline || remaining > redisCacheOperationTimeout {
			test.Errorf("Redis command lacks short cache deadline: deadline=%v remaining=%v", hasDeadline, remaining)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	started := time.Now()
	if value, found := RedisGetContext(context.Background(), "slow"); found || value != "" {
		test.Fatalf("failed Redis read should miss: %q, %v", value, found)
	}
	if err := RedisSetContext(context.Background(), "slow", "value", time.Minute); !errors.Is(err, errRedisCacheUnavailable) {
		test.Fatalf("write did not short-circuit after failed read: %v", err)
	}
	if err := RedisDeleteContext(context.Background(), "slow"); !errors.Is(err, errRedisCacheUnavailable) {
		test.Fatalf("delete did not short-circuit after failed read: %v", err)
	}
	RedisSet("legacy", "value", time.Minute)
	_, _ = RedisGet("legacy")
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond || calls.Load() != 1 {
		test.Fatalf("cache failure repeated serial timeouts: elapsed=%s calls=%d", elapsed, calls.Load())
	}
	redisCacheCircuit.Lock()
	remainingCooldown := time.Until(redisCacheCircuit.retryAt)
	redisCacheCircuit.Unlock()
	if remainingCooldown < 10*time.Second || remainingCooldown > 30*time.Second {
		test.Fatalf("cache failure cooldown outside the required range: %s", remainingCooldown)
	}
}

func TestRedisContextMissIsNotFailureAndLegacySignaturesWork(test *testing.T) {
	var calls atomic.Int32
	stored := make(map[string]string)
	installRedisContextTestClient(test, func(_ context.Context, command redis.Cmder) error {
		calls.Add(1)
		key := command.Args()[1].(string)
		switch command.Name() {
		case "get":
			value, found := stored[key]
			if !found {
				return redis.Nil
			}
			command.(*redis.StringCmd).SetVal(value)
		case "set":
			stored[key] = command.Args()[2].(string)
			command.(*redis.StatusCmd).SetVal("OK")
		case "del":
			delete(stored, key)
			command.(*redis.IntCmd).SetVal(1)
		default:
			test.Fatalf("unexpected command: %s", command.Name())
		}
		return nil
	})
	for range 2 {
		if _, found := RedisGetContext(context.Background(), "key"); found {
			test.Fatal("missing key unexpectedly found")
		}
	}
	RedisSet("key", "value", time.Minute)
	if value, found := RedisGet("key"); !found || value != "value" {
		test.Fatalf("cache miss opened a failure circuit: %q, %v", value, found)
	}
	if err := RedisDeleteContext(context.Background(), "key"); err != nil {
		test.Fatal(err)
	}
	if _, found := RedisGetContext(context.Background(), "key"); found {
		test.Fatal("deleted key was retained")
	}
	if calls.Load() != 6 {
		test.Fatalf("normal misses unexpectedly suppressed commands: calls=%d", calls.Load())
	}
}

func TestRedisContextCallerCancellationDoesNotOpenCircuit(test *testing.T) {
	var calls atomic.Int32
	installRedisContextTestClient(test, func(ctx context.Context, command redis.Cmder) error {
		if calls.Add(1) == 1 {
			deadline, found := ctx.Deadline()
			if !found || time.Until(deadline) > 50*time.Millisecond {
				test.Error("Redis context lost the caller's earlier deadline")
			}
			<-ctx.Done()
			return ctx.Err()
		}
		command.(*redis.StringCmd).SetVal("healthy")
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if value, found := RedisGetContext(ctx, "key"); found || value != "" {
		test.Fatalf("canceled read returned %q, %v", value, found)
	}
	if err := RedisSetContext(ctx, "key", "value", time.Minute); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled write returned %v", err)
	}
	if err := RedisDeleteContext(ctx, "key"); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled delete returned %v", err)
	}
	if calls.Load() != 0 {
		test.Fatal("already canceled callers contacted Redis")
	}
	shortContext, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if _, found := RedisGetContext(shortContext, "key"); found {
		test.Fatal("expired caller unexpectedly received a result")
	}
	if value, found := RedisGetContext(context.Background(), "key"); !found || value != "healthy" || calls.Load() != 2 {
		test.Fatalf("caller cancellation poisoned the shared Redis circuit: %q, %v, calls=%d", value, found, calls.Load())
	}
}

func TestRedisContextLateSuccessCannotCloseFailureCircuit(test *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	installRedisContextTestClient(test, func(_ context.Context, command redis.Cmder) error {
		calls.Add(1)
		if command.Args()[1] == "delayed" {
			close(entered)
			<-release
			command.(*redis.StringCmd).SetVal("old success")
			return nil
		}
		return errors.New("simulated connection failure")
	})
	go func() {
		defer close(finished)
		_, _ = RedisGetContext(context.Background(), "delayed")
	}()
	<-entered
	_, _ = RedisGetContext(context.Background(), "failure")
	close(release)
	<-finished
	_, _ = RedisGetContext(context.Background(), "after_failure")
	if calls.Load() != 2 {
		test.Fatalf("late success incorrectly closed the cooldown: calls=%d", calls.Load())
	}
}

func TestRedisContextCircuitRetriesAfterCooldown(test *testing.T) {
	var calls atomic.Int32
	installRedisContextTestClient(test, func(_ context.Context, command redis.Cmder) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary failure")
		}
		command.(*redis.StringCmd).SetVal("recovered")
		return nil
	})
	_, _ = RedisGetContext(context.Background(), "first")
	_, _ = RedisGetContext(context.Background(), "blocked")
	redisCacheCircuit.Lock()
	redisCacheCircuit.retryAt = time.Now().Add(-time.Second)
	redisCacheCircuit.Unlock()
	if value, found := RedisGetContext(context.Background(), "retry"); !found || value != "recovered" || calls.Load() != 2 {
		test.Fatalf("cache never recovered after cooldown: %q, %v, calls=%d", value, found, calls.Load())
	}
}
