package inventory

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type createStockRequest struct {
	SKUID    uint `json:"sku_id" binding:"required"`
	Quantity int  `json:"quantity"`
}

type deductRequest struct {
	Quantity int `json:"quantity" binding:"required"`
}

// Handler 是库存模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建库存 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// CreateStock 处理 POST /stocks。
func (h *Handler) CreateStock(c *gin.Context) {
	var req createStockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	stock, err := h.service.Create(req.SKUID, req.Quantity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"stock": stock})
}

// GetStock 处理 GET /stocks/:sku_id。
func (h *Handler) GetStock(c *gin.Context) {
	skuID, err := strconv.ParseUint(c.Param("sku_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SKU ID 格式错误"})
		return
	}

	stock, err := h.service.Get(uint(skuID))
	if err != nil {
		if err == ErrStockNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"stock": stock})
}

// DeductStock 处理 POST /stocks/:sku_id/deduct。
func (h *Handler) DeductStock(c *gin.Context) {
	skuID, err := strconv.ParseUint(c.Param("sku_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SKU ID 格式错误"})
		return
	}

	var req deductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	stock, err := h.service.Deduct(uint(skuID), req.Quantity)
	if err != nil {
		if err == ErrInsufficientStock || err == ErrStockNotFound {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"stock": stock})
}
