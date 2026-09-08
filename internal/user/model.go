package user

import "time"

// User 对应数据库里的 users 表。
// GORM 会根据这个结构体自动创建表，结构体里的字段就是表中的列。
//
// TODO(你来实现)：补上用户名和密码两个字段。
// 要求：
//   - 用户名：登录时靠它查找用户，所以要唯一（gorm 标签 uniqueIndex），且不能为空（not null）；
//   - 密码：只保存加密后的结果，绝不能保存明文；返回给前端时也不要暴露它（json 标签用 "-"）。
type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"type:varchar(191);uniqueIndex;not null" json:"username"`
	//Password为了不暴露给前端，因此JSON标签使用横杠进行替代
	Password  string    `gorm:"type:varchar(255);not null" json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
