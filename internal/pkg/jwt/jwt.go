package jwt

import (
	"errors"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken 表示令牌无效或已过期。
var ErrInvalidToken = errors.New("无效的登录令牌")

// secret 是令牌签名密钥。真实项目应放到配置里，这里先用常量方便学习。
const secret = "shopping_market_secret"

// tokenTTL 是令牌有效期。
const tokenTTL = 24 * time.Hour

// Claims 是令牌里携带的信息。
type Claims struct {
	UserID uint `json:"user_id"`
	jwtlib.RegisteredClaims
}

// Generate 根据用户 ID 签发一个登录令牌。
// TODO(你来实现)：
//   1. 创建 Claims，设置 UserID、IssuedAt、ExpiresAt；
//   2. 用 jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims) 创建令牌；
//   3. 用 SignedString([]byte(secret)) 签名并返回。
func Generate(userID uint) (string, error) {
	_ = jwtlib.NewWithClaims
	_ = secret
	_ = tokenTTL
	return "", errors.New("TODO: 实现令牌签发")
}

// Parse 解析并校验登录令牌，返回令牌里的用户 ID。
// TODO(你来实现)：
//   1. 用 jwtlib.ParseWithClaims 解析，传入 Claims 和密钥回调；
//   2. 校验令牌是否有效，并取出 Claims.UserID。
func Parse(tokenString string) (uint, error) {
	_ = jwtlib.ParseWithClaims
	_ = secret
	return 0, errors.New("TODO: 实现令牌解析")
}
