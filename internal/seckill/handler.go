package seckill

import (
	"errors"
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
		c.JSON(seckillErrorStatus(err), gin.H{"error": err.Error()})
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
		c.JSON(seckillErrorStatus(err), gin.H{"error": err.Error()})
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
		c.JSON(seckillErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"order": order})
}

// seckillErrorStatus 把业务层返回的错误翻译成合适的 HTTP 状态码。
//
// 为什么要专门做这件事：以前这里不管什么错误一律返回 500。
// 但 500 的语义是[服务器出故障了]，客户端看到会去重试、监控看到会去告警；
// 而[库存不足][超出每人限购][request_id 重复][活动没开始]这些是**正常的业务拒绝**，
// 重试一万次结果也一样，属于[这次请求本身不成立]，应该用 4xx 明确告诉调用方。
//
// 状态码的选择：
//   - 400 Bad Request：参数写错了（缺字段、数量为负等），改参数再来；
//   - 404 Not Found：商品或活动不存在；
//   - 409 Conflict：和资源当前状态冲突（库存不够、已经买过、幂等键重复、活动时间不对），
//     意思是[请求没毛病，但现在的状态不允许]，重试没有意义；
//   - 500 Internal Server Error：兜底，只有真正没预料到的错误才落到这里。
func seckillErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrItemNotFound), errors.Is(err, ErrActivityNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInsufficientStock),
		errors.Is(err, ErrPerUserLimitExceeded),
		errors.Is(err, ErrAlreadySeckilled),
		errors.Is(err, ErrDuplicateRequest),
		errors.Is(err, ErrActivityNotStarted),
		errors.Is(err, ErrActivityEnded):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
