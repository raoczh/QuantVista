package common

import "testing"

func TestEncryptRoundTrip(t *testing.T) {
	EncryptionKey = "test-encryption-key-不限长度也能派生"
	defer func() { EncryptionKey = "" }()

	cases := []string{
		"sk-1234567890abcdef",
		"含中文与符号!@#的密钥",
		"x", // 短串
	}
	for _, plain := range cases {
		cipherText, err := Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q) 报错: %v", plain, err)
		}
		if cipherText == plain {
			t.Fatalf("Encrypt(%q) 未改变明文，疑似未加密", plain)
		}
		got, err := Decrypt(cipherText)
		if err != nil {
			t.Fatalf("Decrypt 报错: %v", err)
		}
		if got != plain {
			t.Fatalf("回环不一致：want %q got %q", plain, got)
		}
	}
}

func TestEncryptEmptyStays(t *testing.T) {
	EncryptionKey = "whatever"
	defer func() { EncryptionKey = "" }()
	if c, err := Encrypt(""); err != nil || c != "" {
		t.Fatalf("空串应原样返回空串，got %q err %v", c, err)
	}
	if p, err := Decrypt(""); err != nil || p != "" {
		t.Fatalf("空串应原样返回空串，got %q err %v", p, err)
	}
}

func TestEncryptNonceIsRandom(t *testing.T) {
	EncryptionKey = "k"
	defer func() { EncryptionKey = "" }()
	a, _ := Encrypt("same-plaintext")
	b, _ := Encrypt("same-plaintext")
	if a == b {
		t.Fatal("相同明文两次加密应因随机 nonce 得到不同密文")
	}
}

func TestCryptoRequiresKey(t *testing.T) {
	EncryptionKey = ""
	if _, err := Encrypt("x"); err != ErrEncryptionKeyMissing {
		t.Fatalf("无密钥时 Encrypt 应返回 ErrEncryptionKeyMissing，got %v", err)
	}
	if _, err := Decrypt("AAAA"); err != ErrEncryptionKeyMissing {
		t.Fatalf("无密钥时 Decrypt 应返回 ErrEncryptionKeyMissing，got %v", err)
	}
}

func TestDecryptRejectsTampered(t *testing.T) {
	EncryptionKey = "k1"
	c, _ := Encrypt("secret")
	EncryptionKey = "k2" // 换密钥后应认证失败
	defer func() { EncryptionKey = "" }()
	if _, err := Decrypt(c); err == nil {
		t.Fatal("密钥不匹配时 Decrypt 应报错")
	}
}
