package user

import (
	"gorm.io/gorm"
)

// Repository 定义用户数据的读写接口。
// 好处：业务层只依赖这个接口，将来换数据库或写测试时更容易替换。
type Repository interface {
	Create(u *User) error
	FindByUsername(username string) (*User, error)
}

// repository 是接口的具体实现，用 GORM 操作 MySQL。
type repository struct {
	db *gorm.DB
}

// NewRepository 创建用户仓储。
func NewRepository(db *gorm.DB) Repository {
	return &repository{db: db}
}

// Create 把新用户保存到 users 表。
// TODO(你来实现)：调用 r.db.Create(u) 保存用户，并把它的错误返回。
func (r *repository) Create(u *User) error {
	//使用r.db.Create(u)将用户保存到数据库中
	if err := r.db.Create(u).Error; err != nil {
		return err
	}

	return nil
}

// FindByUsername 按用户名查找用户。
// TODO(你来实现)：
//  1. 先声明一个 user 变量；
//  2. 用 r.db.Where("username = ?", username).First(&user) 查询；
//  3. 如果记录不存在（gorm.ErrRecordNotFound），返回 nil, nil；
//  4. 其他错误要把错误返回出来。
func (r *repository) FindByUsername(username string) (*User, error) {
	var user User

	//使用替换参数的方式查询数据库中是否存在该用户名的用户，若不存在则返回nil
	if err := r.db.Where("username=?", username).First(&user).Error; err != nil {
		//如果错误类型是记录未找到，则返回nil,nil空值
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}
