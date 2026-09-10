package product

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type createRequest struct {
	Name        string  `json:"name" binding:"required"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
}

type createSKURequest struct {
	Spec  string  `json:"spec" binding:"required"`
	Price float64 `json:"price"`
}

// Handler 是商品模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建商品 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Create 处理 POST /products。
func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	p, err := h.service.Create(req.Name, req.Description, req.Price)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"product": p})
}

// List 处理 GET /products。
func (h *Handler) List(c *gin.Context) {
	products, err := h.service.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"products": products})
}

// Get 处理 GET /products/:id。
func (h *Handler) Get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "商品 ID 格式错误"})
		return
	}

	p, err := h.service.Get(uint(id))
	if err != nil {
		if err == ErrProductNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"product": p})
}

// Delete 处理 DELETE /products/:id。
func (h *Handler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "商品 ID 格式错误"})
		return
	}

	if err := h.service.Delete(uint(id)); err != nil {
		if err == ErrProductNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已删除"})
}

// CreateSKU 处理 POST /products/:id/skus。
func (h *Handler) CreateSKU(c *gin.Context) {
	productID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "商品 ID 格式错误"})
		return
	}

	var req createSKURequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	sku, err := h.service.CreateSKU(uint(productID), req.Spec, req.Price)
	if err != nil {
		if err == ErrProductNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"sku": sku})
}
