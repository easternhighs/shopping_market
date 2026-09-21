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
// TODO(你来实现)：把 proxy.Director 填完。它是「改写请求」的地方，
// 每个请求在转发出去之前都会经过它。按下面 4 步做：
//
//  1. 改写目标地址（只改 Scheme 和 Host，不要动 Path 和 Query）：
//     req.URL.Scheme = u.Scheme
//     req.URL.Host = u.Host
//     req.Host = u.Host
//     为什么不能动 Path：/cart/items/3 这条路径是下游路由用来匹配的，
//     改了它，下游就找不到对应的接口了。我们只是换了「发往哪台机器」，不是换接口。
//
//  2. 删掉 hopByHopHeaders 里的头（用 req.Header.Del(名字) 挨个删）。
//
//  3. 把追踪编号带过去：
//     if id := trace.FromContext(req.Context()); id != "" { req.Header.Set(trace.HeaderKey, id) }
//     说明：网关的 trace 中间件已经把编号放进了请求的 context，
//     这里取出来显式塞进转发请求的头里，下游就能接到同一个编号。
//
//  4. 不要覆盖 HeaderUserID。
//     这个头是网关的鉴权中间件加上去的，转发时会自动带过去；
//     你在 Director 里什么都不做，它就已经在下游的请求里了——
//     但正因如此，**下游绝不能信任来自外网的 X-User-Id**，只信任网关加的那个。
//
// 另外注意：Director 里不能调用 c（Gin 的上下文），因为这里只有标准的
// *http.Request。所以我们需要的东西（比如 trace id）必须提前放进请求的 context 里，
// 这正是 trace 中间件要同时写 context 和 Gin 上下文两个地方的原因。
func newReverseProxy(target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("上游地址不合法 %q: %w", target, err)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)

	proxy.Director = func(req *http.Request) {
		// TODO(你来实现)：4 步写在这里。
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
