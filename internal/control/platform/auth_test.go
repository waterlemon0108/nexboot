package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

func TestPasswordHashIsSaltedAndVerifiable(t *testing.T) {
	hash1, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	hash2, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if hash1 == "secret" || hash1 == hash2 {
		t.Fatalf("hashes are not salted: %q %q", hash1, hash2)
	}
	if !VerifyPassword(hash1, "secret") {
		t.Fatal("password should verify")
	}
	if VerifyPassword(hash1, "bad") {
		t.Fatal("bad password verified")
	}
	if VerifyPassword("pbkdf2-sha256$100000$$", "anything") {
		t.Fatal("empty salt/key hash verified")
	}
}

func TestAuthServiceLoginAndVerifyToken(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Users().Create(ctx, domain.User{ID: "user-1", Username: "admin", PasswordHash: hash, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := AuthService{Store: st, Secret: []byte("test-secret"), Now: func() time.Time { return now }}

	result, err := service.Login(ctx, LoginRequest{Username: "admin", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Token == "" || result.User.Username != "admin" || !result.ExpiresAt.After(now) {
		t.Fatalf("result = %#v", result)
	}
	user, err := service.VerifyToken(result.Token)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "user-1" || user.Username != "admin" {
		t.Fatalf("user = %#v", user)
	}

	_, err = service.Login(ctx, LoginRequest{Username: "admin", Password: "bad"})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthServiceEnsureInitialAdmin(t *testing.T) {
	ctx := context.Background()
	st := newImageTestStore(t)
	service := AuthService{Store: st, Secret: []byte("test-secret")}

	if err := service.EnsureInitialAdmin(ctx, "admin", "secret"); err != nil {
		t.Fatal(err)
	}
	user, err := st.Users().GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordHash == "secret" || !VerifyPassword(user.PasswordHash, "secret") {
		t.Fatalf("password hash = %q", user.PasswordHash)
	}
	if err := service.EnsureInitialAdmin(ctx, "other", "secret"); err != nil {
		t.Fatal(err)
	}
	users, err := st.Users().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %#v", users)
	}
}
