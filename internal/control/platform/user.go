package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrUserNameRequired     = errs.Invalid("用户名必填")
	ErrUserPasswordRequired = errs.Invalid("密码必填")
	ErrUserExists           = errs.Conflict("用户已存在")
	ErrUserLastAdmin        = errs.Conflict("不能删除最后一个管理员")
	ErrUserOldPassword      = errs.Invalid("旧密码不正确")
)

type UserService struct {
	Store store.Store
	Now   func() time.Time
}

type UserListResult struct {
	Items []UserInfo `json:"items"`
	Total int        `json:"total"`
}

type CreateUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type UpdateUserRequest struct {
	Username string `json:"username"`
}

type ChangePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s UserService) List(ctx context.Context) (UserListResult, error) {
	users, err := s.Store.Users().List(ctx)
	if err != nil {
		return UserListResult{}, err
	}
	items := make([]UserInfo, 0, len(users))
	for _, user := range users {
		items = append(items, userInfo(user))
	}
	return UserListResult{Items: items, Total: len(items)}, nil
}

func (s UserService) Create(ctx context.Context, req CreateUserRequest) (UserInfo, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return UserInfo{}, ErrUserNameRequired
	}
	if req.Password == "" {
		return UserInfo{}, ErrUserPasswordRequired
	}
	if _, err := s.Store.Users().GetByUsername(ctx, username); err == nil {
		return UserInfo{}, fmt.Errorf("%w: %s", ErrUserExists, username)
	} else if !errs.IsNotFound(err) {
		return UserInfo{}, err
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return UserInfo{}, err
	}
	user := domain.User{
		ID:           "user-" + sanitizeForID(username) + "-" + fmt.Sprintf("%d", s.now().UnixNano()),
		Username:     username,
		PasswordHash: hash,
		CreatedAt:    s.now(),
	}
	if err := s.Store.Users().Create(ctx, user); err != nil {
		return UserInfo{}, err
	}
	return userInfo(user), nil
}

func (s UserService) Update(ctx context.Context, id string, req UpdateUserRequest) (UserInfo, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return UserInfo{}, ErrUserNameRequired
	}
	user, err := s.Store.Users().Get(ctx, id)
	if err != nil {
		return UserInfo{}, err
	}
	if existing, err := s.Store.Users().GetByUsername(ctx, username); err == nil && existing.ID != id {
		return UserInfo{}, fmt.Errorf("%w: %s", ErrUserExists, username)
	} else if err != nil && !errs.IsNotFound(err) {
		return UserInfo{}, err
	}
	user.Username = username
	if err := s.Store.Users().Update(ctx, user); err != nil {
		return UserInfo{}, err
	}
	return userInfo(user), nil
}

func (s UserService) Delete(ctx context.Context, id string) error {
	if _, err := s.Store.Users().Get(ctx, id); err != nil {
		return err
	}
	users, err := s.Store.Users().List(ctx)
	if err != nil {
		return err
	}
	if len(users) <= 1 {
		return ErrUserLastAdmin
	}
	return s.Store.Users().Delete(ctx, id)
}

func (s UserService) ChangePassword(ctx context.Context, id string, req ChangePasswordRequest) error {
	if req.NewPassword == "" {
		return ErrUserPasswordRequired
	}
	user, err := s.Store.Users().Get(ctx, id)
	if err != nil {
		return err
	}
	if !VerifyPassword(user.PasswordHash, req.OldPassword) {
		return ErrUserOldPassword
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	return s.Store.Users().Update(ctx, user)
}

func (s UserService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func userInfo(user domain.User) UserInfo {
	return UserInfo{ID: user.ID, Username: user.Username}
}

var _ interface {
	List(context.Context) (UserListResult, error)
	Create(context.Context, CreateUserRequest) (UserInfo, error)
	Update(context.Context, string, UpdateUserRequest) (UserInfo, error)
	Delete(context.Context, string) error
	ChangePassword(context.Context, string, ChangePasswordRequest) error
} = UserService{}
