package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

// fakeUsers — минимальный репозиторий пользователей для тестов.
type fakeUsers struct {
	mu     sync.Mutex
	byMail map[string]*model.User
	next   int64
}

func newFakeUsers() *fakeUsers { return &fakeUsers{byMail: map[string]*model.User{}} }

func (f *fakeUsers) CreateUser(_ context.Context, u *model.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byMail[u.Email]; ok {
		return storage.ErrConflict
	}
	f.next++
	u.ID = f.next
	u.CreatedAt = time.Now()
	c := *u
	f.byMail[u.Email] = &c
	return nil
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byMail[email]; ok {
		c := *u
		return &c, nil
	}
	return nil, storage.ErrNotFound
}

func (f *fakeUsers) GetByID(_ context.Context, id int64) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byMail {
		if u.ID == id {
			c := *u
			return &c, nil
		}
	}
	return nil, storage.ErrNotFound
}

func newSvc() (*Service, *time.Time) {
	s := New(newFakeUsers(), []byte("test-secret"), time.Hour, bcrypt.MinCost)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestRegisterAndLogin(t *testing.T) {
	s, _ := newSvc()
	ctx := context.Background()
	u, tok, err := s.Register(ctx, "  User@Example.COM ", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "user@example.com" || u.ID == 0 || u.PasswordHash == "password123" {
		t.Errorf("пользователь: %+v", u)
	}
	if id, err := s.ParseToken(tok); err != nil || id != u.ID {
		t.Errorf("токен регистрации: id=%d err=%v", id, err)
	}
	u2, tok2, err := s.Login(ctx, "USER@example.com", "password123")
	if err != nil || u2.ID != u.ID {
		t.Fatalf("вход: %v", err)
	}
	if id, err := s.ParseToken(tok2); err != nil || id != u.ID {
		t.Errorf("токен входа: %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	s, _ := newSvc()
	ctx := context.Background()
	if _, _, err := s.Register(ctx, "not-an-email", "password123"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("email: %v", err)
	}
	if _, _, err := s.Register(ctx, "Bob <bob@example.com>", "password123"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("email с именем: %v", err)
	}
	if _, _, err := s.Register(ctx, "a@example.com", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("короткий пароль: %v", err)
	}
	if _, _, err := s.Register(ctx, "a@example.com", strings.Repeat("x", 73)); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("длинный пароль: %v", err)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	s, _ := newSvc()
	ctx := context.Background()
	if _, _, err := s.Register(ctx, "a@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Register(ctx, "A@example.com", "password456"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("дубль: %v", err)
	}
}

func TestLoginFailures(t *testing.T) {
	s, _ := newSvc()
	ctx := context.Background()
	if _, _, err := s.Register(ctx, "a@example.com", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "a@example.com", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("неверный пароль: %v", err)
	}
	if _, _, err := s.Login(ctx, "nobody@example.com", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("нет пользователя: %v", err)
	}
}

func TestToken(t *testing.T) {
	s, now := newSvc()
	tok := s.issue(42)
	if id, err := s.ParseToken(tok); err != nil || id != 42 {
		t.Fatalf("валидный токен: id=%d err=%v", id, err)
	}

	// Подделка: подмена полезной нагрузки при старой подписи.
	other := s.issue(43)
	forged := strings.SplitN(other, ".", 2)[0] + "." + strings.SplitN(tok, ".", 2)[1]
	// Подпись другим секретом.
	foreign := New(newFakeUsers(), []byte("other"), time.Hour, bcrypt.MinCost)
	foreign.now = s.now
	for name, bad := range map[string]string{
		"подмена":    forged,
		"чужой ключ": foreign.issue(42),
		"пустой":     "",
		"без точки":  "abc",
		"мусор":      "!!!.???",
	} {
		if _, err := s.ParseToken(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: ожидалась ErrInvalidToken, получено %v", name, err)
		}
	}

	// Срок действия.
	*now = now.Add(time.Hour - time.Second)
	if _, err := s.ParseToken(tok); err != nil {
		t.Errorf("до истечения: %v", err)
	}
	*now = now.Add(time.Second)
	if _, err := s.ParseToken(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("просроченный: %v", err)
	}
}
