// Package cache описывает кэш горячих ссылок.
// Реализации: RedisCache (продакшен) и MemoryCache (тесты, демо-режим).
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss — ключа нет в кэше.
var ErrMiss = errors.New("cache: miss")

// Entry — минимальные данные, нужные для редиректа без обращения к БД.
type Entry struct {
	LinkID      int64      `json:"id"`
	OriginalURL string     `json:"url"`
	ExpiresAt   *time.Time `json:"exp,omitempty"`
}

// LinkCache — кэш ссылок и счётчики популярности.
type LinkCache interface {
	// Get возвращает запись по коду или ErrMiss.
	Get(ctx context.Context, code string) (*Entry, error)
	// Set кладёт запись с заданным TTL.
	Set(ctx context.Context, code string, e Entry, ttl time.Duration) error
	// Delete удаляет запись (при деактивации ссылки).
	Delete(ctx context.Context, code string) error
	// Hit увеличивает счётчик обращений к коду в скользящем окне window
	// и возвращает новое значение. Используется для решения «кэшировать ли».
	Hit(ctx context.Context, code string, window time.Duration) (int64, error)
	// TopCodes возвращает до n самых популярных кодов за всё время (по убыванию).
	TopCodes(ctx context.Context, n int) ([]string, error)
	Ping(ctx context.Context) error
	Close() error
}
