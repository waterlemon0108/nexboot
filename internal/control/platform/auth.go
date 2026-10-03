package platform

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

var (
	ErrInvalidCredentials = errs.Unauthorized("invalid username or password")
	ErrAuthSecretRequired = errors.New("jwt secret is required")
)

const (
	passwordHashVersion = "pbkdf2-sha256"
	passwordIterations  = 100_000
	passwordSaltBytes   = 16
	passwordKeyBytes    = 32
)

type AuthService struct {
	Store  store.Store
	Secret []byte
	TTL    time.Duration
	Now    func() time.Time
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      UserInfo  `json:"user"`
}

type UserInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type authClaims struct {
	UserID   string `json:"uid"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func (s AuthService) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return LoginResult{}, ErrInvalidCredentials
	}
	user, err := s.Store.Users().GetByUsername(ctx, username)
	if errs.IsNotFound(err) {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}
	if !VerifyPassword(user.PasswordHash, req.Password) {
		return LoginResult{}, ErrInvalidCredentials
	}
	return s.issue(user)
}

func (s AuthService) VerifyToken(tokenText string) (UserInfo, error) {
	if len(s.Secret) == 0 {
		return UserInfo{}, ErrAuthSecretRequired
	}
	var claims authClaims
	token, err := jwt.ParseWithClaims(tokenText, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %s", token.Method.Alg())
		}
		return s.Secret, nil
	}, jwt.WithTimeFunc(s.now))
	if err != nil || !token.Valid {
		return UserInfo{}, ErrInvalidCredentials
	}
	if claims.UserID == "" || claims.Username == "" {
		return UserInfo{}, ErrInvalidCredentials
	}
	return UserInfo{ID: claims.UserID, Username: claims.Username}, nil
}

func (s AuthService) EnsureInitialAdmin(ctx context.Context, username, password string) error {
	users, err := s.Store.Users().List(ctx)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return fmt.Errorf("initial admin username and password are required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	now := s.now()
	return s.Store.Users().Create(ctx, domain.User{
		ID:           "user-" + sanitizeForID(username),
		Username:     username,
		PasswordHash: hash,
		CreatedAt:    now,
	})
}

func (s AuthService) issue(user domain.User) (LoginResult, error) {
	if len(s.Secret) == 0 {
		return LoginResult{}, ErrAuthSecretRequired
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := s.now()
	expires := now.Add(ttl)
	claims := authClaims{
		UserID:   user.ID,
		Username: user.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.Secret)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: token, ExpiresAt: expires, User: UserInfo{ID: user.ID, Username: user.Username}}, nil
}

func (s AuthService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2SHA256([]byte(password), salt, passwordIterations, passwordKeyBytes)
	return fmt.Sprintf("%s$%d$%s$%s",
		passwordHashVersion,
		passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordHashVersion {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	return hmac.Equal(got, want)
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	for block := 1; block <= numBlocks; block++ {
		u := pbkdf2Block(password, salt, iter, block)
		out = append(out, u...)
	}
	return out[:keyLen]
}

func pbkdf2Block(password, salt []byte, iter, block int) []byte {
	mac := hmac.New(sha256.New, password)
	_, _ = mac.Write(salt)
	_, _ = mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
	u := mac.Sum(nil)
	out := append([]byte{}, u...)
	for i := 1; i < iter; i++ {
		mac = hmac.New(sha256.New, password)
		_, _ = mac.Write(u)
		u = mac.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

var _ interface {
	Login(context.Context, LoginRequest) (LoginResult, error)
	VerifyToken(string) (UserInfo, error)
} = AuthService{}
