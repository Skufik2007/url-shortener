// Package storage описывает интерфейсы постоянного хранилища.
// Реализации: storage/postgres (продакшен) и storage/memory (тесты, демо-режим).
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
)

var (
	// ErrNotFound — запись не найдена (или не принадлежит пользователю).
	ErrNotFound = errors.New("storage: not found")
	// ErrConflict — нарушение уникальности (код ссылки, email).
	ErrConflict = errors.New("storage: conflict")
)

// LinkRepository — хранилище коротких ссылок.
type LinkRepository interface {
	// Create сохраняет ссылку, заполняет ID и CreatedAt. ErrConflict, если код занят.
	Create(ctx context.Context, l *model.Link) error
	// GetByCode возвращает ссылку по коду (включая неактивные). ErrNotFound, если нет.
	GetByCode(ctx context.Context, code string) (*model.Link, error)
	// ListByOwner возвращает активные ссылки владельца, новые первыми.
	ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]model.Link, error)
	// Deactivate помечает ссылку неактивной. ErrNotFound, если ссылки нет или она чужая.
	Deactivate(ctx context.Context, ownerID int64, code string) error
	// AddClicks увеличивает счётчик переходов ссылок: linkID -> delta.
	AddClicks(ctx context.Context, deltas map[int64]int64) error
}

// UserRepository — хранилище пользователей.
type UserRepository interface {
	// CreateUser сохраняет пользователя, заполняет ID и CreatedAt. ErrConflict, если email занят.
	CreateUser(ctx context.Context, u *model.User) error
	GetByEmail(ctx context.Context, email string) (*model.User, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
}

// ClickRepository — хранилище переходов и агрегаты для аналитики.
type ClickRepository interface {
	// InsertBatch пакетно сохраняет переходы.
	InsertBatch(ctx context.Context, clicks []model.Click) error
	// Stats считает агрегаты по ссылке за полуинтервал [from, to).
	// Списки By* отсортированы по убыванию Count (ByDay — по дате) и
	// ограничены topN элементами (кроме ByDay).
	Stats(ctx context.Context, linkID int64, from, to time.Time, topN int) (*model.LinkStats, error)
}

// Store объединяет все репозитории; его реализуют postgres.Store и memory.Store.
type Store interface {
	LinkRepository
	UserRepository
	ClickRepository
	Ping(ctx context.Context) error
	Close() error
}
