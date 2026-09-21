package gateway

import (
	"os"
	"strings"
)

// Route 表示「某个路径前缀归哪个服务管」。
//
// 这张表是这一阶段最重要的产出一——比代码重要。
// 因为「怎么拆」本质上就是回答「哪些接口归谁」，把这张表写清楚了，
// 拆进程只是把 Service 从「本地调用的函数」换成「一个上游地址」而已。
type Route struct {
	Prefix  string // 路径前缀，例如 "/cart"
	Service string // 服务名，例如 "order"
}

// Routes 是网关的路由表。
//
// 怎么读这张表：一个请求进来，拿它的路径和下面的 Prefix 逐个比对，
// 命中最长的那一条（最长前缀匹配），就知道该转给哪个服务。
// 比如 "/cart/items/3" 命中 "/cart"，归 order 服务。
//
// 为什么现在就要按服务分好、而不是「全都转给后端」：
// 因为服务边界是设计出来的，不是拆的时候现想的。表先立在这儿，
// 后面每拆一个服务，只要给它配一个独立的上游地址即可。
var Routes = []Route{
	{Prefix: "/login", Service: "user"},
	{Prefix: "/register", Service: "user"},
	{Prefix: "/me", Service: "user"},

	{Prefix: "/products", Service: "product"},
	{Prefix: "/stocks", Service: "product"},

	{Prefix: "/cart", Service: "order"},
	{Prefix: "/orders", Service: "order"},
	{Prefix: "/payments", Service: "order"},

	{Prefix: "/seckill", Service: "seckill"},
	{Prefix: "/metrics", Service: "seckill"},

	{Prefix: "/shop", Service: "shop"},
	{Prefix: "/demo", Service: "console"},
}

// PublicPaths 是不用登录就能访问的路径前缀（白名单）。
//
// 为什么需要白名单：网关要替所有服务做鉴权，那它就必须知道「哪些接口本来就不需要登录」。
// 漏掉一个，页面就 401 打不开；多写一个，就是一个能绕过登录的安全漏洞。
//
// 这份表的来源是各模块的路由注册代码，核对方法很简单：
// 看谁注册在 r（不带鉴权）上，谁注册在 protected（带鉴权）上。
// 例如 internal/user/router.go 里 /login 和 /register 注册在 r 上，所以它们是公开的。
//
// 注意 /shop/my（我的订单）不在白名单里：它是 /shop 下面的私有接口，
// 所以下面单独用一个 ProtectedPaths 把它摘出来。
var PublicPaths = []string{
	"/login",
	"/register",
	"/products",
	"/stocks",
	"/shop",
	"/demo",
	"/metrics",
}

// ProtectedPaths 是「藏在公开前缀下面的私有接口」。
//
// 判断顺序是「先看这个名单，再看白名单」。如果反过来，/shop/my 会先命中 /shop 被放行，
// 那就把别人的订单列表暴露给任何人了。
var ProtectedPaths = []string{
	"/shop/my",
}

// MatchRoute 按最长前缀匹配，找出这个路径该归哪个服务。
//
// TODO(你来实现)：遍历 Routes，返回命中的那一条（第二个返回值 true）；一条都没命中就返回 false。
//
// 关键点：必须选「最长的那个前缀」，不能「随便命中一个就返回」。
// 举例说明为什么：假如以后加了 {Prefix: "/products"} 和 {Prefix: "/products/export"} 两条，
// 请求 /products/export/1 应该归后者管。如果按表里的顺序返回第一个命中的，
// 结果就取决于「表里谁先写」——这种 bug 非常隐蔽，换个顺序就复现或者消失。
//
// 提示：用 strings.HasPrefix(path, route.Prefix) 判断命中；
// 用一个变量记下「目前命中的最长前缀长度」和「对应的那条路由」，遍历完再返回。
func MatchRoute(path string) (Route, bool) {
	// TODO(你来实现)
	_ = path
	return Route{}, false
}

// IsPublic 判断某个路径是不是可以匿名访问。
//
// TODO(你来实现)：分两步判断，顺序不能反：
//  1. 先看 ProtectedPaths：命中就返回 false（私有，需要登录）；
//  2. 再看 PublicPaths：命中就返回 true（公开）；
//  3. 两边都没命中：返回 false（默认要登录）。
//
// 为什么「默认要登录」：安全上的默认值应该选最保守的那个。
// 新加了一个接口、忘了配白名单，最坏结果是「需要登录才能用」（用户抱怨一句）；
// 反过来默认公开、忘了配，那就是「谁都能用」（安全事故）。
//
// 提示：两个名单都用 strings.HasPrefix 判断前缀。
func IsPublic(path string) bool {
	// TODO(你来实现)
	_ = path
	return false
}

// ServiceUpstream 返回某个服务的上游地址。
//
// 规则：先看环境变量 GATEWAY_UPSTREAM_<服务名大写>，没有就用默认地址（defaultBase）。
// 例如：
//
//	GATEWAY_UPSTREAM_USER=http://127.0.0.1:8082   → 拆出去的 user 服务
//	其他没配的服务                                → 都指回同一个后端
//
// 这就是「先拆一个服务」最省事的地方：一个环境变量就把 user 的流量挪走了，
// 代码和路由表都不用改。
func ServiceUpstream(service, defaultBase string) string {
	key := "GATEWAY_UPSTREAM_" + strings.ToUpper(service)
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return defaultBase
}
