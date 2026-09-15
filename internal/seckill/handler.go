package seckill

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

const contextUserID = "userID"

type createActivityRequest struct {
	Name    string `json:"name" binding:"required"`
	StartAt string `json:"start_at" binding:"required"`
	EndAt   string `json:"end_at" binding:"required"`
}

type createItemRequest struct {
	SKUID        uint    `json:"sku_id" binding:"required"`
	Price        float64 `json:"price"`
	TotalStock   int     `json:"total_stock" binding:"required"`
	PerUserLimit int     `json:"per_user_limit" binding:"required"`
}

type seckillRequest struct {
	RequestID string `json:"request_id" binding:"required"`
	Quantity  int    `json:"quantity" binding:"required"`
}

// Handler 是秒杀模块的 HTTP 层。
type Handler struct {
	service *Service
}

// NewHandler 创建秒杀 HTTP 层。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// CreateActivity 处理 POST /seckill/activities。
func (h *Handler) CreateActivity(c *gin.Context) {
	var req createActivityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	startAt, err := time.Parse(time.RFC3339, req.StartAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "start_at 时间格式错误"})
		return
	}
	endAt, err := time.Parse(time.RFC3339, req.EndAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "end_at 时间格式错误"})
		return
	}

	activity, err := h.service.CreateActivity(req.Name, startAt, endAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"activity": activity})
}

// ListActivities 处理 GET /seckill/activities。
func (h *Handler) ListActivities(c *gin.Context) {
	activities, err := h.service.ListActivities()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"activities": activities})
}

// CreateItem 处理 POST /seckill/activities/:id/items。
func (h *Handler) CreateItem(c *gin.Context) {
	activityID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "活动 ID 格式错误"})
		return
	}

	var req createItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	item, err := h.service.CreateItem(uint(activityID), req.SKUID, req.Price, req.TotalStock, req.PerUserLimit)
	if err != nil {
		if err == ErrActivityNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"item": item})
}

// ListItems 处理 GET /seckill/activities/:id/items。
func (h *Handler) ListItems(c *gin.Context) {
	activityID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "活动 ID 格式错误"})
		return
	}

	items, err := h.service.ListItems(uint(activityID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// Seckill 处理 POST /seckill/items/:item_id/orders。
func (h *Handler) Seckill(c *gin.Context) {
	userID := c.GetUint(contextUserID)
	itemID, err := strconv.ParseUint(c.Param("item_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "秒杀商品 ID 格式错误"})
		return
	}

	var req seckillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求格式错误"})
		return
	}

	order, err := h.service.Seckill(userID, uint(itemID), req.RequestID, req.Quantity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"order": order})
}
