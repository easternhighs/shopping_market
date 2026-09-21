// Package trace 提供「全链路追踪编号（trace id）」的最小实现。
//
// 要解决什么问题：一个请求从网关进来，可能穿过好几个服务。出了故障时，日志里躺着几万行，
// 用户只说了一句「我下单失败了」，怎么知道哪几行是同一个请求留下的？
// 办法是给每个请求发一个唯一编号，让它跟着请求一起走完整条链路：
//
//	浏览器 → 网关（生成，或者沿用上游传来的）→ 后端服务 → 日志 → 下游服务 → Kafka 消息头
//
// 只要每个环节都把这个编号打进日志，排查时搜一个编号，整条链路就串起来了。
//
// 这个文件只做三件小事：
//  1. 生成一个编号（NewID）；
//  2. 把编号挂到 context 上、再取出来（WithID / FromContext）；
//  3. 一个 Gin 中间件（Middleware）：从请求头取编号（没有就新生成一个），
//     存进上下文，并把它写回响应头。
//
// 为什么还要写回响应头：这样用 curl 或者浏览器开发者工具就能直接看到这次请求的编号，
// 拿着它去日志里搜，不用猜、也不用问别人要。
//
// 为什么编号不直接在服务内部生成、非要让上游传：一次下单可能跨三个服务，
// 各自生成各自的编号，就没法把它们串成一条链路了。
package trace

import (
	"context"

	"github.com/gin-gonic/gin"
)

// HeaderKey 是 trace id 在 HTTP 头和 Kafka 消息头里统一使用的名字。
// 传输层用同一个名字，服务之间接力时就不用做字段映射。
const HeaderKey = "X-Trace-Id"

// ginKey 是把它放进 Gin 上下文时用的键。
const ginKey = "traceID"

// ctxKey 是放进标准库 context 时用的键。
// 用自定义的空结构体类型，而不是字符串 "traceID"：字符串键容易和其他包撞车，
// 撞了之后两边互相覆盖，排查起来非常难受。
type ctxKey struct{}

// NewID 生成一个新的 trace id。
//
// TODO(你来实现)：返回一个随机、几乎不可能重复的字符串。
// 建议做法：取 16 个随机字节，用十六进制表示（32 个字符）。
//
// 提示：
//   - 随机数用 crypto/rand，不要用 math/rand。math/rand 的序列是可预测的，
//     换个说法：两台机器在同一毫秒启动，可能生成一模一样的编号，那这次追踪就白做了。
//   - 生成失败（实际上几乎不可能）时不要 panic。trace id 只是排查工具，
//     不能因为它把正常的业务请求搞挂——退回一个「时间戳 + 一点随机」的字符串就够了。
//   - 需要新 import 的包：crypto/rand、encoding/hex、time。
func NewID() string {
	// TODO(你来实现)：生成 16 字节随机数 → 转成十六进制字符串。

	// 下面这行只是让函数先能编译通过，填完之后请删掉它。
	return ""
}

// WithID 把编号放进 context，返回一个新的 context。
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext 从 context 里取出编号；没有就返回空字符串。
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// FromGin 从 Gin 上下文里取出编号；没有就返回空字符串。
func FromGin(c *gin.Context) string {
	return c.GetString(ginKey)
}

// Middleware 是给 Gin 用的追踪中间件。
//
// TODO(你来实现)：把下面这段注释里的步骤写成代码。
//
// 它要做四件事，顺序很重要：
//  1. 先看请求头 c.GetHeader(HeaderKey)：
//     有值就沿用（说明上游已经生成过，同一个请求必须用同一个编号）；
//     空字符串就说明这是链路的起点，调用 NewID() 新生成一个。
//  2. 把编号存进两个地方：
//     c.Set(ginKey, id)  —— 给同一次请求里的其他中间件和 handler 用；
//     c.Request = c.Request.WithContext(WithID(c.Request.Context(), id))
//     —— 给那些只拿到 context 的函数用（比如接下来要去调下游服务的时候）。
//  3. c.Header(HeaderKey, id)：写回响应头，让调用方也能看到这次请求的编号。
//     注意这一步必须在 c.Next() 之前做：响应头要在业务写响应体之前设好，
//     等 c.Next() 回来再设，响应可能已经发出去了，设了也看不到。
//  4. c.Next() 放行，让请求继续往后走。
//
// 一个容易踩的坑：这一步不是「一定要新生成」，而是「有就沿用、没有才生成」。
// 如果无脑 NewID()，那链路每经过一跳就换一次编号，等于白做。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// TODO(你来实现)：四步写在这里。

		c.Next()
	}
}
