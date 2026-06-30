package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// ErrEncryptionKeyMissing 表示未配置 ENCRYPTION_KEY，无法加解密敏感字段。
var ErrEncryptionKeyMissing = errors.New("ENCRYPTION_KEY 未配置，无法加解密敏感字段")

// deriveKey 由 EncryptionKey 经 SHA256 派生为固定 32 字节，适配 AES-256。
// .env.example 用 `openssl rand -base64 36` 生成的是 48 字符串而非 32 原始字节，
// 统一做 SHA256 KDF：任意长度的主密钥都能得到合规的 256-bit key。
func deriveKey() ([]byte, bool) {
	if EncryptionKey == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(EncryptionKey))
	return sum[:], true
}

// Encrypt 用 AES-256-GCM 加密明文，返回 base64(nonce|密文|认证标签)。
// 空串原样返回空串（无值字段不产生密文）；未配置 ENCRYPTION_KEY 时报错。
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	key, ok := deriveKey()
	if !ok {
		return "", ErrEncryptionKeyMissing
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	// Seal 把 nonce 作为前缀附在密文前，Decrypt 时再切出来。
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 Encrypt 产生的 base64 密文，空串原样返回。
func Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	key, ok := deriveKey()
	if !ok {
		return "", ErrEncryptionKeyMissing
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足，疑似数据损坏")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", err // 认证失败：密钥不匹配或密文被篡改
	}
	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
