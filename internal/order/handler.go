package order

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const contextUserID = "userID"

type placeRequest struct {
	RequestID string      `json:"request_id" binding:"required"`
	Items     []PlaceItem `json:"items" binding:"required"`
}

// Handler 是订单模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建订单 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Place 处理 POST /orders。
//
// 传进去的是 c.Request.Context() 而不是别的：订单服务现在要跨进程调库存服务，
// 这个 context 里带着两样关键东西——
//   - trace id（中间件塞进去的）：传给下游，两边日志才能对上号；
//   - 请求的超时与取消：用户关掉页面、网关超时，都会顺着它传到下游那次调用，
//     不会出现「用户早走了，我们还在傻等库存服务」。
func (h *Handler) Place(c *gin.Context) {
	userID := c.GetUint(contextUserID)

	var req placeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误", "code": "bad_request"})
		return
	}

	o, err := h.service.Place(c.Request.Context(), userID, req.RequestID, req.Items)
	if err != nil {
		switch {
		case errors.Is(err, ErrStockInsufficient):
			// 库存不够是【业务上的正常结果】，不是服务器故障，所以用 409
			//（冲突：你想要的库存状态和现实冲突了），而不是 500。
			// 带上机器可读的 code，前端就不用去猜中文文案。
			c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "insufficient_stock"})
		case errors.Is(err, ErrInvalidArgument):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "bad_request"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// 注意这里返回的订单状态可能是 pending，也可能是 pending_stock：
	// 后者表示「订单已经收下了，但库存还在确认中，后台会自动重试」。
	// 这不是失败，所以状态码仍然是 200——静默丢单才是失败。
	c.JSON(http.StatusOK, gin.H{"order": o})
}

// Get 处理 GET /orders/:id。
func (h *Handler) Get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单 ID 格式错误"})
		return
	}

	o, items, err := h.service.Get(uint(id))
	if err != nil {
		if err == ErrOrderNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"order": o, "items": items})
}

// Pay 处理 POST /orders/:id/pay。
func (h *Handler) Pay(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单 ID 格式错误"})
		return
	}

	if err := h.service.Pay(uint(id)); err != nil {
		if err == ErrOrderNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if err == ErrInvalidOrderStatus {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "支付成功"})
}

// Cancel 处理 POST /orders/:id/cancel。
func (h *Handler) Cancel(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单 ID 格式错误"})
		return
	}

	if err := h.service.Cancel(uint(id)); err != nil {
		if err == ErrOrderNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if err == ErrInvalidOrderStatus {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "订单已取消"})
}
