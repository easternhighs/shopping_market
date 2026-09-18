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
func Generate(userID uint) (string, error) {
	//1.新建Claims对象，设置UserID、IssuedAt、ExpiresAt
	var claims = Claims{
		UserID: userID,
		RegisteredClaims: jwtlib.RegisteredClaims{
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
			ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(tokenTTL)),
		},
	}

	//2.使用HS256算法创建一个新的令牌对象，并将Claims作为参数传入
	token := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)

	//3.使用密钥对令牌进行签名，并返回签名后的令牌字符串
	SignedToken, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", err
	}

	return SignedToken, nil
}

// Parse 解析并校验登录令牌，返回令牌里的用户 ID。
func Parse(tokenString string) (uint, error) {
	token, err := jwtlib.ParseWithClaims(tokenString, &Claims{}, func(token *jwtlib.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return 0, err
	}

	//校验令牌是否有效，并取出Claims.UserID
	if claims, ok := token.Claims.(*Claims); ok {
		return claims.UserID, nil
	}
	return 0, ErrInvalidToken
}
