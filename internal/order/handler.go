package order

import (
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
func (h *Handler) Place(c *gin.Context) {
	userID := c.GetUint(contextUserID)

	var req placeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	o, err := h.service.Place(userID, req.RequestID, req.Items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

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
