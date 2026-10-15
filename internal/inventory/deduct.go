package inventory

import (
	"errors"
	"log"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrDeductInProgress 表示「同一个 request_id 正被另一个请求处理」。
// 它不是失败，而是「现在还不知道结果」：调用方过一会儿重试就好。
var ErrDeductInProgress = errors.New("同一个 request_id 正在处理中")

// errClaimTaken 只是 ApplyDeduct 内部用的一次性信号：
// 说明 stock_deducts 上已经有人占了坑（唯一索引挡下了这次插入），需要转去查账本。
var errClaimTaken = errors.New("扣减记录已被占用")

// findDeductRecord 按幂等键查账本；查不到返回 (nil, nil)。
func (r *repository) findDeductRecord(requestID string) (*DeductRecord, error) {
	var rec DeductRecord
	if err := r.db.Where("request_id = ?", requestID).First(&rec).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// outcomeOf 把「账本上记着什么」翻译成「这次请求该回答什么」。
func outcomeOf(rec *DeductRecord) error {
	if rec.Status == DeductApplied {
		return nil
	}
	return ErrInsufficientStock
}

// ApplyDeduct 幂等地扣掉一批库存：同一个 request_id 只会真正扣一次。
//
// 步骤（每一步都有它存在的理由）：
//  1. 先查账本。查到过就直接按上次的结论回答 —— 重试的请求绝不能把库存扣两遍。
//  2. 没查到，就在【一个事务】里「先占坑、再扣减」：
//     - 占坑 = 往 stock_deducts 插一行（request_id 唯一）；
//     - 插不进去说明别人正在处理或已经处理过，转去查账本；
//     - 占坑和扣减在同一个事务里，是这里最关键的一点：万一进程在中间崩了，
//     占坑会跟着一起回滚，不会留下「账本说扣过了、库存其实没动」的假账。
//  3. 扣减按 SKU ID 排序后执行：两笔订单如果都要扣 SKU 3 和 SKU 7，
//     一笔按 3→7、另一笔按 7→3，并发起来就会互相等对方的行锁（数据库死锁）。
//     固定顺序是最省事的解法。
//  4. 库存不足时，把失败原因也记进账本：这个 request_id 以后每次重放都会得到
//     同一个「不够」的答案，不会出现「先说不成功、后说成功」的反复。
func (r *repository) ApplyDeduct(requestID string, items []DeductItem) error {
	if rec, err := r.findDeductRecord(requestID); err != nil {
		return err
	} else if rec != nil {
		return outcomeOf(rec)
	}

	// 复制一份再排序：不直接改调用方传进来的切片，
	// 免得调用方回头看见「自己的 items 顺序怎么变了」。
	sorted := make([]DeductItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SKUID < sorted[j].SKUID })

	err := r.db.Transaction(func(tx *gorm.DB) error {
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&DeductRecord{RequestID: requestID, Status: DeductApplied})
		if claim.Error != nil {
			return claim.Error
		}
		// RowsAffected == 0：唯一索引上已经有这个 request_id 了（没插进去）。
		if claim.RowsAffected == 0 {
			return errClaimTaken
		}

		for _, item := range sorted {
			if err := deductWithDB(tx, item.SKUID, item.Quantity); err != nil {
				return err
			}
		}
		return nil
	})

	switch {
	case err == nil:
		return nil
	case errors.Is(err, errClaimTaken):
		return ErrDeductInProgress
	case errors.Is(err, ErrInsufficientStock):
		r.recordRejection(requestID, err.Error())
		return ErrInsufficientStock
	default:
		return err
	}
}

// recordRejection 把「这一单被拒了」记进账本。
// 用 OnConflict{DoNothing}：万一这一行已经被别的请求写好了，就绝不覆盖它。
// 记账失败只打日志、不影响本次返回值 —— 库存本来就不够，先把「不够」这个结论回给调用方。
func (r *repository) recordRejection(requestID, reason string) {
	if err := r.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&DeductRecord{RequestID: requestID, Status: DeductRejected, LastError: reason}).Error; err != nil {
		log.Printf("[库存] 记录扣减失败结果出错 request_id=%s: %v", requestID, err)
	}
}
