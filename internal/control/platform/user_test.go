package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func TestUserServiceCreateRejectsDuplicateUsername(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	service := UserService{Store: st, Now: func() time.Time { return time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC) }}

	user, err := service.Create(ctx, CreateUserRequest{Username: "admin", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "admin" || user.ID == "" {
		t.Fatalf("user = %#v", user)
	}
	stored, err := st.Users().Get(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == "secret" || !VerifyPassword(stored.PasswordHash, "secret") {
		t.Fatalf("password hash = %q", stored.PasswordHash)
	}

	_, err = service.Create(ctx, CreateUserRequest{Username: "admin", Password: "secret"})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestUserServiceDeleteKeepsLastAdmin(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Users().Create(ctx, domain.User{ID: "user-1", Username: "admin", PasswordHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := UserService{Store: st}

	if err := service.Delete(ctx, "user-1"); !errors.Is(err, ErrUserLastAdmin) {
		t.Fatalf("err = %v", err)
	}
	if err := st.Users().Create(ctx, domain.User{ID: "user-2", Username: "ops", PasswordHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, "user-2"); err != nil {
		t.Fatal(err)
	}
	users, err := st.Users().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].ID != "user-1" {
		t.Fatalf("users = %#v", users)
	}
}

func TestUserServiceChangePasswordRequiresOldPassword(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, domain.User{ID: "user-1", Username: "admin", PasswordHash: hash, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	service := UserService{Store: st}

	err = service.ChangePassword(ctx, "user-1", ChangePasswordRequest{OldPassword: "bad", NewPassword: "new-secret"})
	if !errors.Is(err, ErrUserOldPassword) {
		t.Fatalf("err = %v", err)
	}
	if err := service.ChangePassword(ctx, "user-1", ChangePasswordRequest{OldPassword: "secret", NewPassword: "new-secret"}); err != nil {
		t.Fatal(err)
	}
	user, err := st.Users().Get(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(user.PasswordHash, "new-secret") || VerifyPassword(user.PasswordHash, "secret") {
		t.Fatalf("password hash = %q", user.PasswordHash)
	}
}

func TestUserServiceUpdateRejectsDuplicateUsername(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.Users().Create(ctx, domain.User{ID: "user-1", Username: "admin", PasswordHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, domain.User{ID: "user-2", Username: "ops", PasswordHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := UserService{Store: st}

	_, err = service.Update(ctx, "user-2", UpdateUserRequest{Username: "admin"})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("err = %v", err)
	}
	updated, err := service.Update(ctx, "user-2", UpdateUserRequest{Username: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Username != "operator" {
		t.Fatalf("updated = %#v", updated)
	}
}
