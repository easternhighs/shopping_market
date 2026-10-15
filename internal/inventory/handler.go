package inventory

import (
	"errors"
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

// deductBatchRequest 是订单服务跨进程调过来的请求体（POST /internal/stocks/deduct）。
type deductBatchRequest struct {
	RequestID string       `json:"request_id" binding:"required"`
	Items     []DeductItem `json:"items" binding:"required"`
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

// DeductBatchStock 处理 POST /internal/stocks/deduct：订单服务跨进程来扣库存。
//
// 为什么返回体里要带一个机器可读的 code，而不是只有一句中文 error：
// 调用方（订单服务）要根据「库存不足」和「稍后重试」做完全不同的处理 ——
// 前者作废订单，后者留着继续重试。靠匹配中文文案来判断，改一个字就悄悄失效了。
// code 是两份代码之间的契约，error 才是给人看的。
func (h *Handler) DeductBatchStock(c *gin.Context) {
	var req deductBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误", "code": "bad_request"})
		return
	}
	for _, item := range req.Items {
		if item.Quantity <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "扣减数量必须大于 0", "code": "bad_request"})
			return
		}
	}

	err := h.service.DeductBatch(req.RequestID, req.Items)
	switch {
	case err == nil:
		// 幂等重放也走这里：同一个 request_id 第二次来，我们回的还是「成功」。
		c.JSON(http.StatusOK, gin.H{"status": DeductApplied, "request_id": req.RequestID})
	case errors.Is(err, ErrInsufficientStock):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "insufficient_stock"})
	case errors.Is(err, ErrDeductInProgress):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "deduct_in_progress"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "code": "internal_error"})
	}
}
