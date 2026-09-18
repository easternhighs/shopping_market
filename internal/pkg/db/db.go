package db

import (
	"log"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// DefaultDSN 是连接 MySQL 的信息。
// 解释：root:root 是用户名和密码；127.0.0.1:3307 是地址和端口；
// shopping_market 是数据库名；后面的参数让中文和日期时间处理正确。
const DefaultDSN = "root:root@tcp(127.0.0.1:3307)/shopping_market?charset=utf8mb4&parseTime=True&loc=Local"

// Connect 连接 MySQL，返回一个数据库对象。
// 后续用户模块、商品模块等都通过这个对象读写数据库。
func Connect() (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(DefaultDSN), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// 连接池配置：限制同时最多打开多少条数据库连接，避免高并发时把 MySQL 的连接数打满；
	// 同时保留一部分空闲连接复用，省掉每次新建连接的开销。
	dataBase, err := db.DB()
	if err != nil {
		return nil, err
	}

	var (
		MaxOpenConns    int           = 100
		MaxIdleConns    int           = 50
		ConnMaxLifeTime time.Duration = time.Hour
	)

	dataBase.SetMaxOpenConns(MaxOpenConns)
	dataBase.SetMaxIdleConns(MaxIdleConns)
	dataBase.SetConnMaxLifetime(ConnMaxLifeTime)

	log.Println("已连接 MySQL")
	return db, nil
}
