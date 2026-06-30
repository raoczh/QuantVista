package common

import (
	"crypto/rand"
	"math/big"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RandomString 生成 n 位 base62 随机串（crypto/rand，可用于 state/nonce/临时口令）。
func RandomString(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(base62)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 失败极罕见；退回固定字符避免 panic。
			b[i] = base62[0]
			continue
		}
		b[i] = base62[idx.Int64()]
	}
	return string(b)
}
