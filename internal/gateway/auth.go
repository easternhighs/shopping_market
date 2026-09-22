package gateway

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/pkg/jwt"
)

// HeaderUserID 是网关解析完令牌后，写给下游服务的「用户是谁」的头。
//
// 为什么用请求头传递，而不是让下游再解析一遍令牌：
//   - 少解析一次：令牌校验只需要在入口做一次，下游拿到的就是结论；
//   - 少发一份密钥：JWT 现在是 HS256（对称密钥），谁能验谁就能签。
//     密钥只放在网关、下游只认请求头，泄密面小得多；
//   - 换鉴权方式时只改网关：以后换 Cookie、换 OAuth，下游一行都不用动。
//
// 代价必须说清楚：下游从此「无条件相信这个头」。所以生产环境的下游服务
// 必须只在内网监听、不允许外网直连，否则任何人手写一个 X-User-Id 就能冒充登录。
// 这一点写在 docs/microservices.md 的「鉴权下沉」一节里。
const HeaderUserID = "X-User-Id"

// Auth 是网关这一层的鉴权中间件。
//
// 它做的事情和 internal/user/auth.go 里的 Auth 几乎一样（都是「Bearer 令牌 → 用户 ID」），
// 区别只有一个：网关不把用户 ID 只放进自己的上下文，它还要【写进请求头】传给下游，
// 让下游知道「这次请求是谁发的」。
//
// 它按 5 步工作：
//  0. 先删掉客户端自己传来的 X-User-Id（关键，见下面注释）；
//  1. 白名单放行：页面、登录注册接口本来就允许匿名访问；
//  2. 取 "Authorization: Bearer xxx" 里的令牌；
//  3. 用 jwt.Parse 验签并拿到用户 ID；
//  4. 把用户 ID 写进请求头 X-User-Id 传给下游，同时放进网关自己的上下文。
//
// 第 0 步为什么必须做：网关是唯一有权写 X-User-Id 的地方，
// 但公开路径（比如 /shop）是直接放行的，如果不主动删掉这个头，
// 客户端就能自己伪造一个 X-User-Id: 15，让它原样转发给下游。
// 现在下游还不看这个头所以没出事，等拆完 user 服务、下游改成
// 「无条件相信网关的头」，这里就是一个能直接冒充别人的漏洞。
//
// 返回错误一律用 c.AbortWithStatusJSON（不是 c.JSON），
// 否则后面的 handler 还会照常执行——鉴权失败却继续干活，等于没鉴权。
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 0. 先删掉客户端伪造的 X-User-Id（理由见函数上方的说明）。
		c.Request.Header.Del(HeaderUserID)

		// 1. 白名单放行。这一步必须放在最前面——页面、登录接口本来就允许匿名访问，
		//    不放行的话用户连登录页都打不开，会出现「必须先登录才能登录」的笑话。
		if IsPublic(c.Request.URL.Path) {
			c.Next()
			return
		}

		// 2. 取令牌：必须是 "Authorization: Bearer xxx" 的形式。
		authHeader := c.GetHeader("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
			return
		}

		// 3. 解析令牌，拿到用户 ID。
		userID, err := jwt.Parse(strings.TrimPrefix(authHeader, "Bearer "))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
			return
		}

		// 4. 把用户 ID 传给下游，两个地方都要写：
		//    请求头会随着反向代理一起转发给下游服务；
		//    网关自己（比如日志）也能从上下文里拿。
		//    jwt.Parse 返回的是 uint，所以用 FormatUint。
		c.Request.Header.Set(HeaderUserID, strconv.FormatUint(uint64(userID), 10))
		c.Set("userID", userID)

		// 5. 放行。
		c.Next()
	}
}
