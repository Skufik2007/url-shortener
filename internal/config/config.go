// Package config читает конфигурацию сервиса из переменных окружения.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// minSecretLen — минимальная длина секрета подписи токенов в байтах.
const minSecretLen = 32

// Config — все настройки сервиса.
type Config struct {
	HTTPAddr    string
	BaseURL     string // без завершающего «/»
	DatabaseURL string // пусто — хранилище в памяти
	RedisURL    string // пусто — кэш в памяти

	AuthSecret      string
	GeneratedSecret bool // секрет сгенерирован автоматически (демо-режим)
	TokenTTL        time.Duration

	GeoIPDB    string
	TrustProxy bool
	CodeLength int

	AnalyticsWorkers int
	AnalyticsBuffer  int
	AnalyticsBatch   int
	AnalyticsFlush   time.Duration

	RateLimitRPS   float64
	RateLimitBurst int

	LogLevel string
}

// Load читает конфигурацию из окружения процесса.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom читает конфигурацию через произвольную функцию получения значения
// (удобно для тестов).
func LoadFrom(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	integer := func(key string, def, min int) int {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < min {
			errs = append(errs, fmt.Errorf("%s: ожидается целое число >= %d, получено %q", key, min, raw))
			return def
		}
		return n
	}
	duration := func(key string, def time.Duration) time.Duration {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: ожидается положительная длительность (например 1s), получено %q", key, raw))
			return def
		}
		return d
	}

	c := Config{
		HTTPAddr:         str("HTTP_ADDR", ":8080"),
		BaseURL:          strings.TrimRight(str("BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:      str("DATABASE_URL", ""),
		RedisURL:         str("REDIS_URL", ""),
		AuthSecret:       getenv("AUTH_SECRET"),
		TokenTTL:         duration("TOKEN_TTL", 72*time.Hour),
		GeoIPDB:          str("GEOIP_DB", ""),
		CodeLength:       integer("CODE_LENGTH", 7, 4),
		AnalyticsWorkers: integer("ANALYTICS_WORKERS", 4, 1),
		AnalyticsBuffer:  integer("ANALYTICS_BUFFER", 10000, 1),
		AnalyticsBatch:   integer("ANALYTICS_BATCH", 500, 1),
		AnalyticsFlush:   duration("ANALYTICS_FLUSH", time.Second),
		RateLimitBurst:   integer("RATE_LIMIT_BURST", 20, 1),
		LogLevel:         strings.ToLower(str("LOG_LEVEL", "info")),
	}

	if raw := strings.TrimSpace(getenv("TRUST_PROXY")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUST_PROXY: ожидается true/false, получено %q", raw))
		}
		c.TrustProxy = b
	}

	c.RateLimitRPS = 5
	if raw := strings.TrimSpace(getenv("RATE_LIMIT_RPS")); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f <= 0 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_RPS: ожидается положительное число, получено %q", raw))
		} else {
			c.RateLimitRPS = f
		}
	}

	if c.CodeLength > 32 {
		errs = append(errs, errors.New("CODE_LENGTH: максимум 32"))
	}
	if u, err := url.Parse(c.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		errs = append(errs, fmt.Errorf("BASE_URL: ожидается абсолютный http(s)-адрес, получено %q", c.BaseURL))
	}
	if _, err := ParseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}

	// Секрет подписи токенов.
	switch {
	case c.AuthSecret == "" && c.DatabaseURL == "":
		// Демо-режим: данные всё равно не переживут перезапуск, токены тоже.
		buf := make([]byte, 48)
		if _, err := rand.Read(buf); err != nil {
			errs = append(errs, fmt.Errorf("генерация AUTH_SECRET: %w", err))
		}
		c.AuthSecret = base64.RawURLEncoding.EncodeToString(buf)
		c.GeneratedSecret = true
	case c.AuthSecret == "":
		errs = append(errs, errors.New("AUTH_SECRET обязателен, если задан DATABASE_URL"))
	case len(c.AuthSecret) < minSecretLen:
		errs = append(errs, fmt.Errorf("AUTH_SECRET слишком короткий: %d байт, нужно не меньше %d", len(c.AuthSecret), minSecretLen))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// ParseLevel переводит текстовый уровень логирования в slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("LOG_LEVEL: неизвестный уровень %q (debug|info|warn|error)", s)
}
