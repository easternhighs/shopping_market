package gateway

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/gin-gonic/gin"

	"shopping_market/internal/pkg/trace"
)

// hopByHopHeaders 是「只对当前这一跳有意义、不能转发给下一跳」的请求头。
//
// 为什么必须删掉它们：这些头描述的是一条 TCP 连接上的行为。
// 比如 Connection: keep-alive 是说「我和『网关』这条连接要保持」，如果原样转给下游，
// 下游会以为是「和『它自己』这条连接要复用」，语义就串了。
// 这些头由 Go 的 http 包自己维护，反向代理必须把它们摘掉，这是标准要求。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Proxy 负责按路由表把请求转给对应的服务。
type Proxy struct {
	// handlers 的键是服务名，值是「转发到那个服务」的处理函数。
	// 同一个服务只构造一次代理对象，不能每个请求都 new 一个——
	// 反向代理内部带着连接池，每次新建等于每次丢掉连接池，性能会很差。
	handlers map[string]gin.HandlerFunc

	routes []Route
}

// NewProxy 按「服务名 → 上游地址」的对应关系，给每个服务建好一个转发器。
func NewProxy(upstreams map[string]string) (*Proxy, error) {
	handlers := make(map[string]gin.HandlerFunc, len(upstreams))
	for service, target := range upstreams {
		rp, err := newReverseProxy(target)
		if err != nil {
			return nil, err
		}
		handlers[service] = func(c *gin.Context) { rp.ServeHTTP(c.Writer, c.Request) }
	}
	return &Proxy{handlers: handlers, routes: Routes}, nil
}

// Handler 返回 Gin 的处理函数：按路径找到服务，再转给那个服务的转发器。
//
// 这个函数已经写好了，你可以对照它理解「路由表」是怎么被用起来的。
func (p *Proxy) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		route, ok := MatchRoute(c.Request.URL.Path)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "网关没有为这个路径配置上游服务"})
			c.Abort()
			return
		}

		handler, ok := p.handlers[route.Service]
		if !ok {
			c.JSON(http.StatusBadGateway, gin.H{"error": "服务 " + route.Service + " 没有可用的上游地址"})
			c.Abort()
			return
		}

		handler(c)
	}
}

// newReverseProxy 构造一个指向 target 的反向代理。
//
// 改写请求用的是 Rewrite，不是 Director：
//   - Director 已被官方标记为「将来会被 Rewrite 取代」，Go 1.20 起推荐用 Rewrite；
//   - Rewrite 的入参是 httputil.ProxyRequest，它把「进来的请求(In)」和
//     「要发出去的请求(Out)」分得很清楚，不像 Director 那样直接在原请求上改；
//   - 附带好处：用 Rewrite 时反向代理会先替我们删掉客户端自己传的
//     Forwarded / X-Forwarded-* 头，伪造来源这件事默认就被堵住了。
//
// 改写分 4 步（都写在下面的 Rewrite 里）：
//
//  1. 改目标地址：只改 Scheme 和 Host，**不动 Path 和 Query**。
//     为什么不能动 Path：/cart/items/3 是下游用来匹配路由的，改了它下游就找不到
//     对应接口了——我们只是换「发往哪台机器」，不是换接口。
//
//  2. 删掉 hopByHopHeaders 里的头。这些头只对「当前这一跳」有意义
//     （比如 Connection: keep-alive 是说「我和网关这条连接要复用」），
//     原样转给下游语义就串了，所以必须摘干净。
//
//  3. 把 trace id 带过去：网关的 trace 中间件已经把编号放进了请求的 context，
//     这里取出来显式塞进转发请求的头里，下游才能接着用同一个编号打日志。
//     注意取的是 req.In.Context()，也就是「进来的那个请求」的 context。
//
//  4. 不动 X-User-Id：它是网关的鉴权中间件刚加上去的，而 Out 是 In 的克隆，
//     会自动带过去，这里什么都不用做。
//
// 同样注意：Rewrite 里不能调用 c（Gin 的上下文），这里只有标准的 *http.Request。
// 所以需要的东西（比如 trace id）必须提前放进请求的 context 里，
// 这正是 trace 中间件要同时写 context 和 Gin 上下文两个地方的原因。
func newReverseProxy(target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("上游地址不合法 %q: %w", target, err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)

	// 关键的一行：NewSingleHostReverseProxy 默认会帮我们填一个 Director，
	// 而 ReverseProxy 要求 Director 和 Rewrite 【只能有一个】。
	// 两个都在，每个请求都会直接失败：
	//   ReverseProxy must have exactly one of Director or Rewrite set
	// 表现就是所有请求都回 502「上游服务不可用」，而后端其实是好的。
	// 既然改用 Rewrite，就必须把默认的 Director 清掉。
	proxy.Director = nil

	// Rewrite 在每个请求转发出去之前执行，是「改写请求」的地方。
	proxy.Rewrite = func(req *httputil.ProxyRequest) {
		// 1. 改目标地址：只改 Scheme / Host，Path 和 Query 原样保留。
		req.Out.URL.Scheme = u.Scheme
		req.Out.URL.Host = u.Host
		req.Out.Host = u.Host

		// 2. 摘掉 hop-by-hop 头，它们只对当前这一跳有意义。
		for _, key := range hopByHopHeaders {
			req.Out.Header.Del(key)
		}

		// 3. 透传 trace id，下游就能用同一个编号打日志。
		if id := trace.FromContext(req.In.Context()); id != "" {
			req.Out.Header.Set(trace.HeaderKey, id)
		}

		// 4. 这里刻意不调用 req.SetXForwarded()。
		//    它会把 X-Forwarded-For 设成「客户端传来的值 + 真实 IP」，
		//    而下游取客户端 IP 用的是 c.ClientIP()，它读的是 XFF 的第一段。
		//    客户端只要每次换一个 XFF，就能每次都换一个限流桶，把限流绕过去。
		//    本项目的下游（也就是现在这个单体）不需要知道真实客户端 IP，
		//    所以干脆不加这个头。将来真要加，正确做法是「覆盖」而不是「追加」：
		//      req.Out.Header.Set("X-Forwarded-For", 真实客户端 IP)
		//
		//    X-User-Id 同理不在这里处理：Out 是 In 的克隆，
		//    鉴权中间件刚写进去的那个头会自动带过去。
	}

	// ErrorHandler 已经写好：上游连不上（服务没起来、端口写错、网络不通）时，
	// 返回 502 而不是 500，并且带上 trace id——排查时能一眼看出是「网关的问题」还是「后端的问题」。
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		id := trace.FromContext(r.Context())
		log.Printf("[网关] 转发失败 target=%s path=%s trace=%s err=%v", target, r.URL.Path, id, err)
		w.Header().Set(trace.HeaderKey, id)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"上游服务不可用"}`))
	}

	return proxy, nil
}
