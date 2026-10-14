// Package auth реализует регистрацию, вход и подписанные HMAC-токены.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

var (
	ErrInvalidCredentials = errors.New("auth: неверный email или пароль")
	ErrEmailTaken         = errors.New("auth: email уже зарегистрирован")
	ErrInvalidToken       = errors.New("auth: недействительный токен")
	ErrWeakPassword       = errors.New("auth: пароль должен быть от 8 до 72 байт")
	ErrInvalidEmail       = errors.New("auth: некорректный email")
)

const (
	minPassword = 8
	maxPassword = 72 // предел bcrypt
)

// Service — регистрация, вход и проверка токенов.
type Service struct {
	users  storage.UserRepository
	secret []byte
	ttl    time.Duration
	cost   int
	now    func() time.Time
	// dummyHash — фиктивный хэш для выравнивания времени входа несуществующего пользователя.
	dummyHash []byte
}

// New создаёт сервис. bcryptCost вне допустимого диапазона заменяется на DefaultCost.
func New(users storage.UserRepository, secret []byte, ttl time.Duration, bcryptCost int) *Service {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		bcryptCost = bcrypt.DefaultCost
	}
	dummy, _ := bcrypt.GenerateFromPassword([]byte("dummy-password"), bcryptCost)
	return &Service{users: users, secret: secret, ttl: ttl, cost: bcryptCost, now: time.Now, dummyHash: dummy}
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Name != "" || addr.Address != email {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// Register создаёт пользователя и возвращает его вместе с токеном.
func (s *Service) Register(ctx context.Context, email, password string) (*model.User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return nil, "", err
	}
	if len(password) < minPassword || len(password) > maxPassword {
		return nil, "", ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost)
	if err != nil {
		return nil, "", fmt.Errorf("auth: хэширование пароля: %w", err)
	}
	u := &model.User{Email: email, PasswordHash: string(hash)}
	if err := s.users.CreateUser(ctx, u); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return nil, "", ErrEmailTaken
		}
		return nil, "", err
	}
	return u, s.issue(u.ID), nil
}

// Login проверяет пароль и возвращает пользователя с новым токеном.
func (s *Service) Login(ctx context.Context, email, password string) (*model.User, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Сравнение с фиктивным хэшем — защита от перечисления email по времени ответа.
			_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, "", ErrInvalidCredentials
	}
	return u, s.issue(u.ID), nil
}

func (s *Service) sign(payload string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// issue выпускает токен: base64url("id.exp") + "." + base64url(HMAC).
func (s *Service) issue(userID int64) string {
	exp := s.now().Add(s.ttl).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d", userID, exp)))
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

// ParseToken проверяет подпись и срок действия и возвращает ID пользователя.
func (s *Service) ParseToken(token string) (int64, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return 0, ErrInvalidToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(payload)) {
		return 0, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return 0, ErrInvalidToken
	}
	idStr, expStr, ok := strings.Cut(string(raw), ".")
	if !ok {
		return 0, ErrInvalidToken
	}
	id, err1 := strconv.ParseInt(idStr, 10, 64)
	exp, err2 := strconv.ParseInt(expStr, 10, 64)
	if err1 != nil || err2 != nil || !s.now().Before(time.Unix(exp, 0)) {
		return 0, ErrInvalidToken
	}
	return id, nil
}
