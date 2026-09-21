package gateway

import (
	"github.com/gin-gonic/gin"
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
// TODO(你来实现)：按下面 5 步把这个中间件写完。
//
// 动手之前先在文件顶部补上这几个 import（现在只留了 gin，其他都靠你自己加）：
//
//		"net/http"                            401 状态码
//		"strconv"                             把 userID 转成字符串
//		"strings"                             处理 "Bearer " 前缀
//		"shopping_market/internal/pkg/jwt"    解析令牌
//
//	 1. 白名单放行：如果 IsPublic(c.Request.URL.Path) 为 true，就 c.Next() 然后 return。
//	    这一步必须放在最前面——页面、登录接口本来就允许匿名访问，
//	    不放行的话用户连登录页都打不开，会出现「必须先登录才能登录」的笑话。
//
//	 2. 取令牌：authHeader := c.GetHeader("Authorization")，
//	    判断是否以 "Bearer " 开头，不是就返回 401 + c.Abort()。
//	    （可以直接抄 internal/user/auth.go，那段逻辑写得很清楚了。）
//
//	 3. 解析令牌：jwt.Parse(token) 拿到 userID；出错同样返回 401 + c.Abort()。
//
//	 4. 把用户 ID 传给下游，两个地方都要写：
//	    c.Request.Header.Set(HeaderUserID, strconv.FormatUint(uint64(userID), 10))
//	    —— 这一步是关键：请求头会随着反向代理一起转发给下游服务；
//	    c.Set("userID", userID)
//	    —— 网关自己如果需要用到（比如日志），也能从上下文里拿。
//	    strconv 需要新 import。
//
//	 5. c.Next() 放行。
//
// 注意返回错误的写法：c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
// 一定要带 Abort，否则后面的 handler 还会照常执行——鉴权失败却继续干活，等于没鉴权。
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// TODO(你来实现)：5 步写在这里。

		c.Next()
	}
}
