package product

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type findSKUInternalRequest struct {
	ProductID uint   `json:"product_id" binding:"required"`
	Spec      string `json:"spec" binding:"required"`
}

// RegisterInternalRoutes 注册只给兄弟服务使用的商品接口。
//
// 这些路径都带 /internal 前缀。网关的路由表不认识这个前缀，
// 所以浏览器经网关访问会得到 404；真上线还应再加内网监听或服务间认证。
func RegisterInternalRoutes(r *gin.Engine, h *Handler) {
	r.GET("/internal/products/skus", h.ListSKUsInternal)
	r.POST("/internal/products/skus/find", h.FindSKUInternal)
}

// ListSKUsInternal 处理 GET /internal/products/skus?product_ids=1,2,3。
//
// 一次请求拿多个商品的规格，避免调用方按商品逐个发请求（N+1 次网络往返）。
func (h *Handler) ListSKUsInternal(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("product_ids"))
	if raw == "" {
		c.JSON(http.StatusOK, gin.H{"skus": []SKU{}})
		return
	}

	parts := strings.Split(raw, ",")
	ids := make([]uint, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}

		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "product_ids 格式错误"})
			return
		}
		ids = append(ids, uint(id))
	}

	skus, err := h.service.ListSKUsByProductIDs(ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"skus": skus})
}

// FindSKUInternal 处理 POST /internal/products/skus/find。
//
// 「找不到」是正常结果，所以返回 200 + {"sku":null}，而不是 404。
// 调用方（控制台造数）拿到 null 后会走「创建 SKU」分支；
// 如果这里返回 404，调用方还得把「真不存在」和「商品服务坏了」分开，
// 反而多一层容易写错的判断。
func (h *Handler) FindSKUInternal(c *gin.Context) {
	var req findSKUInternalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	sku, err := h.service.FindSKUByProductAndSpec(req.ProductID, req.Spec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sku": sku})
}
