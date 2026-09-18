package user

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/pkg/jwt"
)

// contextUserID 是鉴权中间件往上下文里存放用户 ID 的键。
const contextUserID = "userID"

// Auth 是登录鉴权中间件：从请求头里取出令牌，解析出用户 ID。
func (h *Handler) Auth(c *gin.Context) {
	//1.获取请求头中的Authorization字段
	authHeader := c.GetHeader("Authorization")

	//2.判断Authorization字段是否以"Bearer "开头，如果不是则返回401错误。若是则去掉前缀。
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")

	//3.调用之前写的jwt处理函数处理token，获取userID
	userID, err := jwt.Parse(token)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
		return
	}
	//4.如果token有效，则将userID存入上下文中，并继续处理请求
	c.Set(contextUserID, userID)
	c.Next()
}
