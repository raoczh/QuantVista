package common

import (
	"crypto/rand"
	"math/big"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RandomString 生成 n 位 base62 随机串（crypto/rand，用于 refresh token/state/nonce）。
// 熵源失败直接 panic——静默退化成固定字符会签发出可枚举的令牌，比崩溃危险得多
// （与 jwt.go fallbackSecret 的口径一致）。
func RandomString(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(base62)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("系统熵源不可用，无法生成安全随机串: " + err.Error())
		}
		b[i] = base62[idx.Int64()]
	}
	return string(b)
}
