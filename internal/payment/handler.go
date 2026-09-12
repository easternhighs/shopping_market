package payment

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const contextUserID = "userID"

// Handler 是支付模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建支付 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Callback 处理 POST /payments/:order_id/callback。
func (h *Handler) Callback(c *gin.Context) {
	userID := c.GetUint(contextUserID)
	orderID, err := strconv.ParseUint(c.Param("order_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单 ID 格式错误"})
		return
	}

	p, err := h.service.Callback(userID, uint(orderID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"payment": p})
}

// Get 处理 GET /payments/:order_id。
func (h *Handler) Get(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("order_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单 ID 格式错误"})
		return
	}

	p, err := h.service.Get(uint(orderID))
	if err != nil {
		if err == ErrPaymentNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"payment": p})
}
