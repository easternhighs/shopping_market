package user

import (
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"shopping_market/internal/pkg/jwt"
)

var (
	// ErrUsernameTaken 表示用户名已经被别人注册了。
	ErrUsernameTaken = errors.New("用户名已存在")
	// ErrInvalidCredentials 表示用户名或密码错误。
	ErrInvalidCredentials = errors.New("用户名或密码错误")
)

// Service 是用户模块的业务层，负责注册、登录等业务规则。
type Service struct {
	repo Repository
}

// NewService 创建用户业务层。
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Register 注册新用户。
// 流程：检查参数 -> 检查用户名是否已存在 -> 密码加密 -> 保存。
func (s *Service) Register(username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("用户名和密码不能为空")
	}

	existing, err := s.repo.FindByUsername(username)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrUsernameTaken
	}

	hashed, err := hashPassword(password)
	if err != nil {
		return nil, err
	}

	u := &User{
		Username: username,
		Password: hashed,
	}
	if err := s.repo.Create(u); err != nil {
		return nil, err
	}

	return u, nil
}

// Login 校验用户名和密码，成功后返回登录令牌。
func (s *Service) Login(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", errors.New("用户名和密码不能为空")
	}

	u, err := s.repo.FindByUsername(username)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", ErrInvalidCredentials
	}

	if err := checkPassword(u.Password, password); err != nil {
		return "", ErrInvalidCredentials
	}

	token, err := jwt.Generate(u.ID)
	if err != nil {
		return "", err
	}

	return token, nil
}

// GetByID 按用户 ID 查询用户。
func (s *Service) GetByID(id uint) (*User, error) {
	return s.repo.FindByID(id)
}

// checkPassword 校验明文密码是否和加密密码匹配。
func checkPassword(hashedPassword, password string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password)); err != nil {
		return errors.New("密码错误")
	}
	return nil
}

// hashPassword 把明文密码加密。
func hashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	return string(hashed), nil
}
