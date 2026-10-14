// Package memory содержит потокобезопасную реализацию storage.Store в памяти.
// Используется в тестах и в демо-режиме (когда DATABASE_URL не задан).
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

var _ storage.Store = (*Store)(nil)

// Store — хранилище в памяти.
type Store struct {
	mu sync.RWMutex

	nextLinkID  int64
	nextUserID  int64
	nextClickID int64

	links      map[int64]*model.Link
	linkByCode map[string]int64
	users      map[int64]*model.User
	userByMail map[string]int64
	clicks     map[int64][]model.Click // linkID -> переходы
}

// New создаёт пустое хранилище.
func New() *Store {
	return &Store{
		links:      make(map[int64]*model.Link),
		linkByCode: make(map[string]int64),
		users:      make(map[int64]*model.User),
		userByMail: make(map[string]int64),
		clicks:     make(map[int64][]model.Click),
	}
}

// Create сохраняет ссылку. ErrConflict, если код занят.
func (s *Store) Create(ctx context.Context, l *model.Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.linkByCode[l.Code]; ok {
		return storage.ErrConflict
	}
	s.nextLinkID++
	l.ID = s.nextLinkID
	l.CreatedAt = time.Now().UTC()
	cp := *l
	s.links[l.ID] = &cp
	s.linkByCode[l.Code] = l.ID
	return nil
}

// GetByCode возвращает копию ссылки по коду.
func (s *Store) GetByCode(ctx context.Context, code string) (*model.Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.linkByCode[code]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *s.links[id]
	return &cp, nil
}

// ListByOwner возвращает активные ссылки владельца, новые первыми.
func (s *Store) ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]model.Link, error) {
	s.mu.RLock()
	var res []model.Link
	for _, l := range s.links {
		if l.OwnerID == ownerID && l.IsActive {
			res = append(res, *l)
		}
	}
	s.mu.RUnlock()

	sort.Slice(res, func(i, j int) bool {
		if !res[i].CreatedAt.Equal(res[j].CreatedAt) {
			return res[i].CreatedAt.After(res[j].CreatedAt)
		}
		return res[i].ID > res[j].ID
	})
	if offset < 0 {
		offset = 0
	}
	if offset >= len(res) {
		return []model.Link{}, nil
	}
	res = res[offset:]
	if limit >= 0 && limit < len(res) {
		res = res[:limit]
	}
	return res, nil
}

// Deactivate помечает ссылку неактивной. ErrNotFound, если ссылки нет или она чужая.
func (s *Store) Deactivate(ctx context.Context, ownerID int64, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.linkByCode[code]
	if !ok || s.links[id].OwnerID != ownerID || !s.links[id].IsActive {
		return storage.ErrNotFound
	}
	s.links[id].IsActive = false
	return nil
}

// AddClicks увеличивает счётчики переходов; неизвестные ссылки пропускаются.
func (s *Store) AddClicks(ctx context.Context, deltas map[int64]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range deltas {
		if l, ok := s.links[id]; ok {
			l.ClickCount += d
		}
	}
	return nil
}

// CreateUser сохраняет пользователя. ErrConflict, если email занят (без учёта регистра).
func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(u.Email)
	if _, ok := s.userByMail[key]; ok {
		return storage.ErrConflict
	}
	s.nextUserID++
	u.ID = s.nextUserID
	u.CreatedAt = time.Now().UTC()
	cp := *u
	s.users[u.ID] = &cp
	s.userByMail[key] = u.ID
	return nil
}

// GetByEmail возвращает пользователя по email (без учёта регистра).
func (s *Store) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.userByMail[strings.ToLower(email)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *s.users[id]
	return &cp, nil
}

// GetByID возвращает пользователя по ID.
func (s *Store) GetByID(ctx context.Context, id int64) (*model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

// InsertBatch сохраняет переходы.
func (s *Store) InsertBatch(ctx context.Context, clicks []model.Click) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range clicks {
		s.nextClickID++
		c.ID = s.nextClickID
		s.clicks[c.LinkID] = append(s.clicks[c.LinkID], c)
	}
	return nil
}

// Stats считает агрегаты по ссылке за полуинтервал [from, to).
func (s *Store) Stats(ctx context.Context, linkID int64, from, to time.Time, topN int) (*model.LinkStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := &model.LinkStats{From: from, To: to}
	if l, ok := s.links[linkID]; ok {
		st.Code = l.Code
	}

	days := map[time.Time]int64{}
	visitors := map[string]struct{}{}
	country := map[string]int64{}
	referrer := map[string]int64{}
	browser := map[string]int64{}
	osm := map[string]int64{}
	device := map[string]int64{}

	for _, c := range s.clicks[linkID] {
		if c.ClickedAt.Before(from) || !c.ClickedAt.Before(to) {
			continue
		}
		st.TotalClicks++
		visitors[c.VisitorID] = struct{}{}
		t := c.ClickedAt.UTC()
		days[time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)]++
		country[c.Country]++
		referrer[c.Referrer]++
		browser[c.Browser]++
		osm[c.OS]++
		device[c.Device]++
	}
	st.UniqueVisitors = int64(len(visitors))

	st.ByDay = make([]model.DayCount, 0, len(days))
	for d, n := range days {
		st.ByDay = append(st.ByDay, model.DayCount{Day: d, Count: n})
	}
	sort.Slice(st.ByDay, func(i, j int) bool { return st.ByDay[i].Day.Before(st.ByDay[j].Day) })

	st.ByCountry = top(country, topN)
	st.ByReferrer = top(referrer, topN)
	st.ByBrowser = top(browser, topN)
	st.ByOS = top(osm, topN)
	st.ByDevice = top(device, topN)
	return st, nil
}

// top сортирует счётчики по убыванию Count (при равенстве — по Key) и обрезает до n.
func top(m map[string]int64, n int) []model.KeyCount {
	res := make([]model.KeyCount, 0, len(m))
	for k, v := range m {
		res = append(res, model.KeyCount{Key: k, Count: v})
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Count != res[j].Count {
			return res[i].Count > res[j].Count
		}
		return res[i].Key < res[j].Key
	})
	if n >= 0 && n < len(res) {
		res = res[:n]
	}
	return res
}

// Ping всегда успешен.
func (s *Store) Ping(ctx context.Context) error { return nil }

// Close ничего не освобождает.
func (s *Store) Close() error { return nil }
