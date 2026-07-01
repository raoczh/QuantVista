package common

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL access token 有效期（短期，无状态）。refresh token 落库长期、可吊销。
const AccessTokenTTL = 2 * time.Hour

// jwtSecret 返回 JWT 签名密钥。生产环境 SessionSecret 已 fail-fast 保证非空；
// 开发环境为空时退回固定不安全默认值，避免空密钥签名的隐性问题。
func jwtSecret() []byte {
	if SessionSecret != "" {
		return []byte(SessionSecret)
	}
	return []byte("quantvista-dev-insecure-secret")
}

// Claims access token 载荷。
type Claims struct {
	UserID int64  `json:"uid"`
	Role   string `json:"role"`
	Ver    int    `json:"ver"` // 令牌版本，与 User.TokenVersion 比对，用于即时废止
	jwt.RegisteredClaims
}

// IssueAccessToken 签发 HS256 access token，返回 token 串与过期时间。
func IssueAccessToken(userID int64, role string, ver int) (string, time.Time, error) {
	exp := time.Now().Add(AccessTokenTTL)
	claims := Claims{
		UserID: userID,
		Role:   role,
		Ver:    ver,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "quantvista",
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret())
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

// ParseAccessToken 校验并解析 access token。
func ParseAccessToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("非预期的 JWT 签名方法")
		}
		return jwtSecret(), nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token 无效")
	}
	return claims, nil
}
