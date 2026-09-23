package user

import (
	"net/http"
	"os"
	"shopping_market/internal/pkg/jwt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// contextUserID 是鉴权中间件往上下文里存放用户 ID 的键。
// handler（例如 Me）就是从这个键取用户 ID 的，改名要两边一起改。
const contextUserID = "userID"

// HeaderUserID 是网关注入的「这次请求是谁发的」请求头。
//
// 它和 internal/gateway 里的 HeaderUserID 是同一个头（值都是 X-User-Id）。
// 两个包各留一份常量，是因为它们属于两个进程、各自需要自己那一份；
// 名字保持一致，读代码时能一眼对上。
//
// 为什么下游可以只看这个头、不用自己再验一次 JWT：
//   - 网关是唯一入口，签名已经验过了，下游拿到的是结论，省一次 CPU 开销；
//   - JWT 现在是对称密钥（HS256），谁会验谁就能签，密钥少发一份就少一份泄露面；
//   - 以后换鉴权方式（Cookie、OAuth），只改网关，下游一行都不用动。
//
// 代价必须说清楚：这个头本身没有任何防伪能力，谁都能写。
// 所以它只有在【下游只在内网监听、前面挡着网关】时才可信——
// 这正是 TrustHeaderFromEnv 默认返回 false 的原因。
const HeaderUserID = "X-User-Id"

// TrustHeaderFromEnv 读环境变量，决定「要不要信任网关注入的 X-User-Id 头」。
//
// 判断规则：读环境变量 TRUST_USER_ID_HEADER，返回它是不是「开」。
//
// 要求（函数只有几行，但每一条都有理由）：
//  1. 用 os.Getenv("TRUST_USER_ID_HEADER") 取值；
//  2. 先 strings.TrimSpace 去掉首尾空格再判断（容器编排里很容易多出一个空格）；
//  3. 只有 "1" 和 "true" 算开，而且忽略大小写（"TRUE"、"True" 也算开）；
//     其它一切值——空串、没设置、"0"、"yes"、"on"——统统算【关】；
//  4. 返回 true 表示「信任」，false 表示「不信任」。
//
// 为什么默认关（也就是为什么空串/没设置一律算「不信任」）：
// 安全上的默认值要选最保守的那个。同一个二进制可能跑在开发机上、也可能不小心暴露到公网，
// 如果默认信任 X-User-Id，那任何人手写一个请求头就能冒充任意用户——
// 这是能直接翻别人购物车和订单的漏洞。只有明确知道「我前面有网关」时才应该打开它。
//
// 为什么用白名单（"1"/"true" 才算开）而不是黑名单（"0" 才算关）：
// 黑名单是「没写 0 就当开」，打错一个字符（比如写成 "flase"）它就悄悄开了，没人会发现；
// 白名单搞错了最多是「以为开了其实没开」，会立刻表现出来。
//
// 实现：先把值 TrimSpace 掉，再和白名单（只有 "1" 和 "true"）逐字比对。
func TrustHeaderFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv("TRUST_USER_ID_HEADER"))
	return strings.EqualFold(raw, "1") || strings.EqualFold(raw, "true")
}

// AuthMiddleware 返回「登录鉴权」中间件。
//
// 这个中间件唯一的产出，是把「这次请求是谁」放进 gin 上下文（键是 contextUserID）。
// 后面的 handler（例如 Me）只管从上下文里取，不关心它是怎么来的。
//
// 参数 trustHeader 决定用哪种模式：
//
//	trustHeader = true   进程跑在网关后面：优先采用网关写好的 X-User-Id；
//	                     头里没有值时，才回退去验 JWT（留着方便临时直连排查问题）。
//	trustHeader = false  进程单独启动、直连访问：永远按老规矩验 JWT。
//
// 【第 1 段 · 信任头模式】只在 trustHeader == true 时执行：
//   - 读 c.GetHeader(HeaderUserID)；
//   - 值非空 → 用 strconv.ParseUint(raw, 10, 64) 解析成数字，转成 uint，
//     用 c.Set(contextUserID, userID) 存进上下文，然后 c.Next() 放行，并 return；
//   - 解析失败（有人乱写 "abc"、"-1"、超出范围的大数）→ 直接
//     c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})，
//     然后 return；
//   - 值为空 → 什么都不做，让它自然落到第 2 段去验 JWT。
//
// 【第 2 段 · 回退验 JWT】原来的逻辑，一个条件都不能少：
//   - 取 c.GetHeader("Authorization")，要求形如 "Authorization: Bearer xxx"；
//     没有 "Bearer " 前缀 → AbortWithStatusJSON(401, ...) 并 return；
//   - jwt.Parse(strings.TrimPrefix(authHeader, "Bearer ")) 验签拿 userID；
//     出错 → AbortWithStatusJSON(401, ...) 并 return；
//   - 成功 → c.Set(contextUserID, userID)，再 c.Next()。
//
// ⚠ 三个必须想清楚的点（面试很容易被追问）：
//  1. 第 1 段里「解析失败」不能当成「没有头」继续往下走第 2 段。
//     一个带畸形 X-User-Id 的请求本来就该被拒绝；放它去验 JWT，等于给了它第二条命。
//  2. 不要在这个函数开头 c.Request.Header.Del(HeaderUserID)。
//     网关那一层已经负责「先删掉客户端伪造的、再写进验签结果」了；
//     下游如果自己再删一次，就把自己要读的值删没了，信任头模式直接失效。
//  3. 失败一律用 c.AbortWithStatusJSON，不要用 c.JSON。
//     Abort 之后后面的 handler 才会停下；用 c.JSON 的话鉴权失败了还照样把活干完。
//
// 为什么返回 401 而不是 400/500：401 的语义是「你还没证明你是谁」，
// 正好对应「没登录 / 令牌不对」；400 是「请求格式不对」，500 是「我服务内部出错了」。
func AuthMiddleware(trustHeader bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 第 1 段：进程跑在网关后面时，直接采用网关注入的 X-User-Id。
		if trustHeader {
			rawUserID := c.GetHeader(HeaderUserID)
			if rawUserID != "" {
				parsed, err := strconv.ParseUint(rawUserID, 10, 64)
				if err != nil {
					// 「有值但格式不对」不等于「没有这个头」：这种请求只能拒绝。
					// 放它去走第 2 段，等于给一个畸形请求第二条命。
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
					return
				}

				// 存之前必须转成 uint：下游 handler 是用 v.(uint) 或 c.GetUint 取的，
				// 存 uint64 会因为「类型断言失败」而取不到；GetUint 还会安静地返回 0，
				// 于是受保护接口以「用户 0」的身份放行——页面看着正常，其实全线失守。
				c.Set(contextUserID, uint(parsed))
				c.Next()

				// 这个 return 不能少：gin 里 c.Next() 执行完还会回到这里继续往下走，
				// 少了它就会再进第 2 段，把第二个 JSON 追加到已经写完的响应体后面
				// （响应体变成两个 JSON 拼在一起，是非法 JSON）。
				return
			}
		}

		// 第 2 段：回退到验 JWT。头里没值、或进程本来就不信任这个头时走到这里。
		authHeader := c.GetHeader("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
			return
		}

		userID, err := jwt.Parse(strings.TrimPrefix(authHeader, "Bearer "))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的登录令牌"})
			return
		}

		c.Set(contextUserID, userID)
		c.Next()
	}
}
