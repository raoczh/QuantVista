package service

import (
	"context"
	"sync"
	"time"

	"quantvista/common"
)

// 移动 OAuth 流的服务端一次性存储（阶段 B，设计见 docs/ANDROID_APP_PLAN.md §5.3）：
//   - mstate:<nonce>  → PKCE challenge（TTL 10min）——移动流 state 防重放。Web 流靠
//     HttpOnly cookie double-submit 一次性；移动流发起在 App、回调落在系统浏览器，
//     cookie 不共享，必须换服务端一次性消费。
//   - mcode:<短码>    → userID+challenge（TTL 60s）——回调页换取的一次性短码，
//     App 深链收到后凭 code_verifier 兑换 JWT 双 token。
//
// Redis 可用时 SET EX + GETDEL 原子消费（多实例安全）；不可用时退进程内
// sync.Map + TTL——一次性语义靠 LoadAndDelete 原子性，**仅单实例部署成立**
// （本项目部署形态即单容器，见 docs/DEPLOYMENT.md；扩多实例前必须先上 Redis）。

const oauthOnceKeyPrefix = "qv:oauth:once:"

// oauthOnceStore 包级单例：进程内兜底存储。
var oauthOnceStore = newTTLStore()

// storeOnce 写入一次性记录。
func storeOnce(key, value string, ttl time.Duration) {
	if common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := common.RDB.Set(ctx, oauthOnceKeyPrefix+key, value, ttl).Err(); err == nil {
			return
		}
		// Redis 写失败退进程内，保证登录流程不因缓存组件抖动而中断。
	}
	oauthOnceStore.set(key, value, ttl)
}

// consumeOnce 原子取出并删除一次性记录；不存在或已过期返回 ("", false)。
// 同一 key 并发消费只有一方成功——防 state 重放与短码重放的根基。
func consumeOnce(key string) (string, bool) {
	if common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		v, err := common.RDB.GetDel(ctx, oauthOnceKeyPrefix+key).Result()
		if err == nil {
			return v, true
		}
		// redis.Nil（不存在）与网络错误统一按 miss 处理后再查进程内：
		// 写入时可能因 Redis 抖动落在了兜底存储。
	}
	return oauthOnceStore.consume(key)
}

// ttlStore 进程内 TTL map。写入时惰性清扫过期条目，防长期运行下的无界增长
// （移动登录频次极低，全量扫描代价可忽略）。
type ttlStore struct {
	m sync.Map // key → ttlEntry
}

type ttlEntry struct {
	val string
	exp time.Time
}

func newTTLStore() *ttlStore { return &ttlStore{} }

func (s *ttlStore) set(key, value string, ttl time.Duration) {
	now := time.Now()
	s.m.Range(func(k, v any) bool {
		if e, ok := v.(ttlEntry); ok && now.After(e.exp) {
			s.m.Delete(k)
		}
		return true
	})
	s.m.Store(key, ttlEntry{val: value, exp: now.Add(ttl)})
}

// consume 原子取删（LoadAndDelete），过期条目视同不存在。
func (s *ttlStore) consume(key string) (string, bool) {
	v, ok := s.m.LoadAndDelete(key)
	if !ok {
		return "", false
	}
	e, ok := v.(ttlEntry)
	if !ok || time.Now().After(e.exp) {
		return "", false
	}
	return e.val, true
}
