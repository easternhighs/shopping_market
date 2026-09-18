package user

import "time"

// User 对应数据库里的 users 表。
// GORM 会根据这个结构体自动创建表，结构体里的字段就是表中的列。
type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"type:varchar(191);uniqueIndex;not null" json:"username"`
	//Password为了不暴露给前端，因此JSON标签使用横杠进行替代
	Password  string    `gorm:"type:varchar(255);not null" json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
