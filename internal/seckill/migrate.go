package seckill

import "gorm.io/gorm"

// EnsureOrderIndexes 在启动时修正 seckill_orders 表上的两个索引。
//
// 为什么要单独写一段代码做这件事：结构体上的 tag 只能告诉 GORM
// 「建表时该建什么索引」，AutoMigrate 发现同名索引已经存在就直接跳过，
// 既不会去改它、也不会补建。所以老库上的历史遗留必须在启动时手动修正一次。
//
// 一共两件事，都写成幂等的：已经处于正确状态时什么都不做。
//
//  1. 把 idx_user_item 从「唯一索引」改成「普通索引」。
//     阶段 9 之前建好的库上那个唯一索引必须删掉重建，否则
//     「超时关单之后重新抢购」会插不进去，报唯一键冲突。
//
//  2. 补建 (status, created_at) 复合索引，给超时兜底扫描用。
//     没有这个索引时，`WHERE status='pending' AND created_at < ...` 会退化成
//     全表扫描：压测后表里躺着 440 万行待付款订单、没有一行落在回看窗口内，
//     MySQL 只能一行行扫完整个表才敢说「没找到」，实测每次 1.6~1.8 秒，
//     而兜底扫描每 5 秒就有一轮，等于常年白烧一个 CPU 核去证明「啥也没有」。
//     加上索引后同样的查询是毫秒级：先按 status 定位、再按 created_at 范围跳过。
//
// 注意：在大表上 CREATE INDEX 需要几十秒，期间服务会卡在启动阶段，
// 这属于一次性成本，之后就再也不会执行了。
func EnsureOrderIndexes(db *gorm.DB) error {
	if err := ensureUserItemIndexIsPlain(db); err != nil {
		return err
	}
	return ensureExpireSweepIndex(db)
}

// ensureUserItemIndexIsPlain 把 idx_user_item 上的唯一约束去掉。
func ensureUserItemIndexIsPlain(db *gorm.DB) error {
	const (
		indexName = "idx_user_item"
		tableName = "seckill_orders"
	)

	var uniqueCount int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ? AND non_unique = 0`,
		tableName, indexName,
	).Scan(&uniqueCount).Error; err != nil {
		return err
	}

	if uniqueCount == 0 {
		return nil
	}

	if err := db.Exec("DROP INDEX " + indexName + " ON " + tableName).Error; err != nil {
		return err
	}

	return db.Exec("CREATE INDEX " + indexName + " ON " + tableName + " (item_id, user_id)").Error
}

// ensureExpireSweepIndex 保证超时兜底扫描有索引可用。
func ensureExpireSweepIndex(db *gorm.DB) error {
	const (
		indexName = "idx_status_created"
		tableName = "seckill_orders"
	)

	var count int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM information_schema.statistics
		  WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
		tableName, indexName,
	).Scan(&count).Error; err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	return db.Exec("CREATE INDEX " + indexName + " ON " + tableName + " (status, created_at)").Error
}
