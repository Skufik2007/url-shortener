// Package httpapi содержит HTTP-слой сервиса: маршруты, обработчики,
// middleware, ограничение частоты и метрики.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/service"
)

// Authenticator — то, что HTTP-слою нужно от сервиса аутентификации
// (его реализует *auth.Service).
type Authenticator interface {
	Register(ctx context.Context, email, password string) (*model.User, string, error)
	Login(ctx context.Context, email, password string) (*model.User, string, error)
	ParseToken(token string) (int64, error)
}

// HealthCheck — проверка зависимости для /healthz.
type HealthCheck struct {
	Name string
	Ping func(ctx context.Context) error
}

// Deps — зависимости HTTP-сервера.
type Deps struct {
	Links  *service.LinkService
	Auth   Authenticator
	Health []HealthCheck

	// Metrics необязателен: по умолчанию создаётся новый набор счётчиков.
	Metrics *Metrics
	// CollectorStats возвращает счётчики сборщика аналитики (может быть nil).
	CollectorStats func() CollectorStats

	BaseURL    string
	TrustProxy bool

	// Ограничение частоты для POST /links и /auth/*; RateLimitRPS <= 0 отключает его.
	RateLimitRPS   float64
	RateLimitBurst int

	// Dashboard — файловая система со статикой дашборда (корень с index.html);
	// nil — дашборд не раздаётся.
	Dashboard fs.FS

	Logger *slog.Logger
}

type server struct {
	Deps
	log     *slog.Logger
	limiter *RateLimiter
}

// NewServer собирает http.Handler со всеми маршрутами и middleware.
func NewServer(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Metrics == nil {
		deps.Metrics = NewMetrics()
	}
	deps.BaseURL = strings.TrimRight(deps.BaseURL, "/")
	s := &server{
		Deps:    deps,
		log:     deps.Logger,
		limiter: NewRateLimiter(deps.RateLimitRPS, deps.RateLimitBurst),
	}

	mux := http.NewServeMux()

	// Служебные.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// API.
	mux.HandleFunc("POST /api/v1/auth/register", s.limited(s.handleRegister))
	mux.HandleFunc("POST /api/v1/auth/login", s.limited(s.handleLogin))
	mux.HandleFunc("GET /api/v1/me", s.authed(s.handleMe))
	mux.HandleFunc("POST /api/v1/links", s.limited(s.authed(s.handleCreateLink)))
	mux.HandleFunc("GET /api/v1/links", s.authed(s.handleListLinks))
	mux.HandleFunc("GET /api/v1/links/{code}", s.authed(s.handleGetLink))
	mux.HandleFunc("DELETE /api/v1/links/{code}", s.authed(s.handleDeleteLink))
	mux.HandleFunc("GET /api/v1/links/{code}/stats", s.authed(s.handleStats))
	mux.HandleFunc("GET /api/v1/top", s.handleTop)

	// QR-код короткой ссылки (PNG).
	mux.HandleFunc("GET /qr/{file}", s.handleQR)

	// Дашборд.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
	})
	if deps.Dashboard != nil {
		files := http.StripPrefix("/dashboard/", http.FileServerFS(deps.Dashboard))
		mux.Handle("GET /dashboard/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		}))
	}

	// Переход по короткой ссылке — самый «широкий» шаблон, регистрируется последним.
	mux.HandleFunc("GET /{code}", s.handleRedirect)

	var h http.Handler = mux
	h = limitBody(h)
	h = securityHeaders(h)
	h = recoverer(s.log, h)
	h = accessLog(s.log, s.Metrics, s.TrustProxy, h)
	h = requestID(h)
	return h
}
