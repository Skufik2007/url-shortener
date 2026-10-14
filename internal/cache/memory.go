package cache

import (
	"context"
	"sort"
	"sync"
	"time"
)

type memEntry struct {
	e       Entry
	expires time.Time
}

type memHits struct {
	n       int64
	expires time.Time
}

// MemoryCache — реализация LinkCache в памяти процесса (тесты, демо-режим).
type MemoryCache struct {
	mu      sync.Mutex
	now     func() time.Time // внедряемые часы для тестов
	links   map[string]memEntry
	hits    map[string]memHits
	popular map[string]int64
}

var _ LinkCache = (*MemoryCache)(nil)

// NewMemory создаёт пустой кэш с системными часами.
func NewMemory() *MemoryCache {
	return &MemoryCache{
		now:     time.Now,
		links:   map[string]memEntry{},
		hits:    map[string]memHits{},
		popular: map[string]int64{},
	}
}

// Get возвращает запись или ErrMiss (в том числе если TTL истёк).
func (m *MemoryCache) Get(_ context.Context, code string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.links[code]
	if !ok {
		return nil, ErrMiss
	}
	if !v.expires.IsZero() && !m.now().Before(v.expires) {
		delete(m.links, code)
		return nil, ErrMiss
	}
	e := v.e
	return &e, nil
}

// Set сохраняет запись; ttl <= 0 означает отсутствие срока жизни.
func (m *MemoryCache) Set(_ context.Context, code string, e Entry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = m.now().Add(ttl)
	}
	m.links[code] = memEntry{e: e, expires: exp}
	return nil
}

// Delete удаляет запись.
func (m *MemoryCache) Delete(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, code)
	return nil
}

// Hit увеличивает счётчик окна (окно стартует с первого обращения) и популярность.
func (m *MemoryCache) Hit(_ context.Context, code string, window time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	h, ok := m.hits[code]
	if !ok || (!h.expires.IsZero() && !now.Before(h.expires)) {
		h = memHits{}
		if window > 0 {
			h.expires = now.Add(window)
		}
	}
	h.n++
	m.hits[code] = h
	m.popular[code]++
	return h.n, nil
}

// TopCodes возвращает до n самых популярных кодов (при равенстве — по алфавиту).
func (m *MemoryCache) TopCodes(_ context.Context, n int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	codes := make([]string, 0, len(m.popular))
	for c := range m.popular {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		if m.popular[codes[i]] != m.popular[codes[j]] {
			return m.popular[codes[i]] > m.popular[codes[j]]
		}
		return codes[i] < codes[j]
	})
	if n < 0 {
		n = 0
	}
	if len(codes) > n {
		codes = codes[:n]
	}
	return codes, nil
}

// Ping всегда успешен.
func (m *MemoryCache) Ping(context.Context) error { return nil }

// Close ничего не освобождает.
func (m *MemoryCache) Close() error { return nil }
