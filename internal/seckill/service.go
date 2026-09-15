package seckill

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrActivityNotFound 表示秒杀活动不存在。
	ErrActivityNotFound = errors.New("秒杀活动不存在")
	// ErrItemNotFound 表示秒杀商品不存在。
	ErrItemNotFound = errors.New("秒杀商品不存在")
	// ErrInsufficientStock 表示秒杀库存不足。
	ErrInsufficientStock = errors.New("秒杀库存不足")
	// ErrAlreadySeckilled 表示该用户已经抢购过这个秒杀商品。
	ErrAlreadySeckilled = errors.New("您已经抢购过该商品")
	// ErrDeductUnknown 表示 Redis 扣减结果未知。
	ErrDedectUnknown = errors.New("库存扣减未知错误")
)

// Service 是秒杀模块的业务层。
type Service struct {
	repo  Repository
	cache *Cache
	queue *Queue
}

// NewService 创建秒杀业务层。
func NewService(repo Repository, cache *Cache, queue *Queue) *Service {
	return &Service{repo: repo, cache: cache, queue: queue}
}

// CreateActivity 创建一场秒杀活动。
func (s *Service) CreateActivity(name string, startAt, endAt time.Time) (*Activity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("活动名称不能为空")
	}
	if !endAt.After(startAt) {
		return nil, errors.New("结束时间必须晚于开始时间")
	}

	a := &Activity{Name: name, StartAt: startAt, EndAt: endAt}
	if err := s.repo.CreateActivity(a); err != nil {
		return nil, err
	}
	return a, nil
}

// ListActivities 查询全部秒杀活动。
func (s *Service) ListActivities() ([]Activity, error) {
	return s.repo.ListActivities()
}

// CreateItem 为某场活动添加一个秒杀商品。
func (s *Service) CreateItem(activityID, skuID uint, price float64, totalStock, perUserLimit int) (*Item, error) {
	activity, err := s.repo.FindActivityByID(activityID)
	if err != nil {
		return nil, err
	}
	if activity == nil {
		return nil, ErrActivityNotFound
	}
	if totalStock <= 0 {
		return nil, errors.New("秒杀库存必须大于 0")
	}
	if perUserLimit <= 0 {
		return nil, errors.New("每人限购数量必须大于 0")
	}

	item := &Item{
		ActivityID:   activityID,
		SKUID:        skuID,
		Price:        price,
		TotalStock:   totalStock,
		PerUserLimit: perUserLimit,
	}
	if err := s.repo.CreateItem(item); err != nil {
		return nil, err
	}
	return item, nil
}

// ListItems 查询某场活动下的全部秒杀商品。
func (s *Service) ListItems(activityID uint) ([]Item, error) {
	return s.repo.ListItemsByActivityID(activityID)
}

// Seckill 处理一次秒杀请求。
func (s *Service) Seckill(userID, itemID uint, requestID string, quantity int) (*Order, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, errors.New("requestID 不能为空")
	}
	if quantity <= 0 {
		return nil, errors.New("抢购数量必须大于 0")
	}

	//2.查询秒杀商品和活动，校验活动时间
	item, err := s.repo.FindItemByID(itemID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, ErrItemNotFound
	}

	activity, err := s.repo.FindActivityByID(item.ActivityID)
	if err != nil {
		return nil, err
	}
	if activity == nil {
		return nil, ErrActivityNotFound
	}

	//校验活动时间
	now := time.Now()
	if now.Before(activity.StartAt) {
		return nil, errors.New("秒杀活动尚未开始")
	}
	if now.After(activity.EndAt) {
		return nil, errors.New("秒杀活动已结束")
	}

	//3.先做用户限购与防重复检查
	existingOrder, err := s.repo.FindOrderByRequestID(requestID)
	if err != nil {
		return nil, err
	}
	if existingOrder != nil {
		return nil, errors.New("重复的请求 ID")
	}

	// 同一个用户对同一个秒杀商品只能成功一次，这里先做快速拦截。
	existingUserOrder, err := s.repo.FindOrderByUserAndItem(userID, itemID)
	if err != nil {
		return nil, err
	}
	if existingUserOrder != nil {
		return nil, ErrAlreadySeckilled
	}

	//查询用户已购买数量
	if quantity > item.PerUserLimit {
		return nil, errors.New("超过每人限购数量")
	}

	//4.用 Redis Lua 脚本原子预扣库存
	ctx := context.Background()
	if err := s.cache.PreDeduct(ctx, itemID, userID, quantity, item.TotalStock, item.PerUserLimit); err != nil {
		return nil, err
	}

	//5.成功后发送 Kafka 消息，由消费端最终创建 MySQL 订单
	message := OrderMessage{
		RequestID:  requestID,
		ActivityID: activity.ID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
	}
	if err := s.queue.Publish(ctx, message); err != nil {
		return nil, err
	}

	//6.客户端这里先返回“排队中”的订单标识
	order := &Order{
		RequestID:  requestID,
		ActivityID: activity.ID,
		ItemID:     item.ID,
		SKUID:      item.SKUID,
		UserID:     userID,
		Quantity:   quantity,
		Status:     "queued",
	}
	return order, nil
}
