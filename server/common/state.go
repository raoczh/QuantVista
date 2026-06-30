package common

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// stateTTL OAuth state 有效期。
const stateTTL = 10 * time.Minute

// SignState 生成无状态、防 CSRF 的 OAuth state：nonce.ts.hmac。
// 无需服务端存储——回调时用 HMAC + 时效校验，攻击者无法伪造合法 state。
func SignState() string {
	nonce := RandomString(16)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := nonce + "." + ts
	return payload + "." + stateMAC(payload)
}

// VerifyState 校验 state 的 HMAC 与时效。
func VerifyState(state string) bool {
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return false
	}
	payload := parts[0] + "." + parts[1]
	expect := stateMAC(payload)
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expect)) != 1 {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(ts, 0)) <= stateTTL
}

func stateMAC(payload string) string {
	mac := hmac.New(sha256.New, jwtSecret())
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// SHA256Hex 返回字符串的 sha256 十六进制（用于 refresh token 落库时只存摘要）。
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
