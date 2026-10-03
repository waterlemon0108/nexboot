package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func TestUserRepoGetByUsername(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))

	admin := domain.User{ID: "user-admin", Username: "admin", PasswordHash: "hash-admin", CreatedAt: now}
	if err := st.Users().Create(ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	got, err := st.Users().GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("get by username: %v", err)
	}
	if got.ID != "user-admin" || got.Username != "admin" || got.PasswordHash != "hash-admin" {
		t.Fatalf("got user = %#v", got)
	}
}

func TestUserRepoGetByUsernameNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.Users().GetByUsername(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
