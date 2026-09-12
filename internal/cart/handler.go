package cart

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const contextUserID = "userID"

type addItemRequest struct {
	SKUID    uint `json:"sku_id" binding:"required"`
	Quantity int  `json:"quantity" binding:"required"`
}

type updateQuantityRequest struct {
	Quantity int `json:"quantity" binding:"required"`
}

// Handler 是购物车模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建购物车 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// AddItem 处理 POST /cart/items。
func (h *Handler) AddItem(c *gin.Context) {
	userID := c.GetUint(contextUserID)
	var req addItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	item, err := h.service.AddItem(userID, req.SKUID, req.Quantity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"item": item})
}

// List 处理 GET /cart/items。
func (h *Handler) List(c *gin.Context) {
	userID := c.GetUint(contextUserID)
	items, err := h.service.List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

// UpdateQuantity 处理 PATCH /cart/items/:item_id。
func (h *Handler) UpdateQuantity(c *gin.Context) {
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "明细 ID 格式错误"})
		return
	}

	var req updateQuantityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	if err := h.service.UpdateQuantity(uint(itemID), req.Quantity); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已更新"})
}

// RemoveItem 处理 DELETE /cart/items/:item_id。
func (h *Handler) RemoveItem(c *gin.Context) {
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "明细 ID 格式错误"})
		return
	}

	if err := h.service.RemoveItem(uint(itemID)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}
