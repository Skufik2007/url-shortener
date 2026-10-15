// Package service содержит бизнес-логику сокращения ссылок:
// создание, разрешение кода с кэшированием популярных ссылок,
// удаление и выдачу статистики владельцу.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/shortcode"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

var (
	ErrNotFound   = errors.New("link not found")
	ErrGone       = errors.New("link expired or disabled")
	ErrInvalidURL = errors.New("invalid url")
	ErrAliasTaken = errors.New("alias already taken")
	ErrBadAlias   = errors.New("invalid alias")
	ErrBadPeriod  = errors.New("invalid period")
)

const (
	maxURLLength    = 2048
	maxTitleLength  = 200
	generateRetries = 5
	maxStatsPeriod  = 366 * 24 * time.Hour
)

// Options — настраиваемые параметры сервиса.
type Options struct {
	// PublicHost — хост, на котором работает сервис; ссылки на него запрещены,
	// чтобы нельзя было строить цепочки редиректов.
	PublicHost string
	// PopularThreshold — сколько обращений за HitWindow делают ссылку «популярной»
	// и заставляют положить её в кэш.
	PopularThreshold int64
	HitWindow        time.Duration
	CacheTTL         time.Duration
	StatsTopN        int
}

// DefaultOptions возвращает разумные значения по умолчанию.
func DefaultOptions() Options {
	return Options{
		PopularThreshold: 3,
		HitWindow:        10 * time.Minute,
		CacheTTL:         time.Hour,
		StatsTopN:        10,
	}
}

// LinkService — фасад бизнес-логики.
type LinkService struct {
	store   storage.Store
	cache   cache.LinkCache
	gen     shortcode.Generator
	tracker analytics.Tracker
	opts    Options
	log     *slog.Logger
	now     func() time.Time
}

// New собирает сервис.
func New(store storage.Store, c cache.LinkCache, gen shortcode.Generator,
	tracker analytics.Tracker, opts Options, log *slog.Logger) *LinkService {
	if log == nil {
		log = slog.Default()
	}
	return &LinkService{store: store, cache: c, gen: gen, tracker: tracker,
		opts: opts, log: log, now: time.Now}
}

// CreateInput — параметры создания ссылки.
type CreateInput struct {
	URL       string     `json:"url"`
	Alias     string     `json:"alias,omitempty"`
	Title     string     `json:"title,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Create создаёт короткую ссылку для владельца ownerID.
func (s *LinkService) Create(ctx context.Context, ownerID int64, in CreateInput) (*model.Link, error) {
	target, err := s.normalizeURL(in.URL)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(in.Title)
	if len([]rune(title)) > maxTitleLength {
		title = string([]rune(title)[:maxTitleLength])
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		return nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidURL)
	}

	link := &model.Link{
		OriginalURL: target,
		OwnerID:     ownerID,
		Title:       title,
		ExpiresAt:   in.ExpiresAt,
		IsActive:    true,
	}

	if in.Alias != "" {
		if err := shortcode.ValidateAlias(in.Alias); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadAlias, err)
		}
		link.Code = in.Alias
		if err := s.store.Create(ctx, link); err != nil {
			if errors.Is(err, storage.ErrConflict) {
				return nil, ErrAliasTaken
			}
			return nil, err
		}
		return link, nil
	}

	// Случайный код: при коллизии пробуем ещё раз.
	for i := 0; i < generateRetries; i++ {
		code, err := s.gen.Generate()
		if err != nil {
			return nil, err
		}
		link.Code = code
		err = s.store.Create(ctx, link)
		if err == nil {
			return link, nil
		}
		if !errors.Is(err, storage.ErrConflict) {
			return nil, err
		}
		s.log.Warn("short code collision", "code", code, "attempt", i+1)
	}
	return nil, errors.New("could not generate unique code")
}

func (s *LinkService) normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", ErrInvalidURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: only http and https are allowed", ErrInvalidURL)
	}
	if s.opts.PublicHost != "" && strings.EqualFold(u.Hostname(), hostOnly(s.opts.PublicHost)) {
		return "", fmt.Errorf("%w: links to this service are not allowed", ErrInvalidURL)
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// hostOnly отбрасывает порт и квадратные скобки IPv6: "[::1]:8080" -> "::1".
func hostOnly(h string) string {
	return (&url.URL{Host: h}).Hostname()
}

// Visit — данные запроса, нужные для учёта перехода.
type Visit struct {
	IP          string
	UserAgent   string
	Referer     string
	CountryHint string
}

// Resolve находит адрес назначения по коду и ставит переход в очередь аналитики.
// Сначала смотрит в кэш; при промахе идёт в БД и кэширует ссылку,
// если она стала популярной (PopularThreshold обращений за HitWindow).
func (s *LinkService) Resolve(ctx context.Context, code string, v Visit) (string, error) {
	if !shortcode.LooksValid(code) {
		return "", ErrNotFound
	}
	now := s.now()

	entry, err := s.cache.Get(ctx, code)
	if err != nil {
		if !errors.Is(err, cache.ErrMiss) {
			// Кэш недоступен — деградируем до чтения из БД.
			s.log.Warn("cache get failed", "err", err)
		}
		entry, err = s.loadAndMaybeCache(ctx, code)
		if err != nil {
			return "", err
		}
	}
	if entry.ExpiresAt != nil && !now.Before(*entry.ExpiresAt) {
		return "", ErrGone
	}

	s.tracker.Track(analytics.Event{
		LinkID:      entry.LinkID,
		Time:        now,
		IP:          v.IP,
		UserAgent:   v.UserAgent,
		Referer:     v.Referer,
		CountryHint: v.CountryHint,
	})
	return entry.OriginalURL, nil
}

func (s *LinkService) loadAndMaybeCache(ctx context.Context, code string) (*cache.Entry, error) {
	link, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !link.IsActive || link.Expired(s.now()) {
		return nil, ErrGone
	}
	entry := &cache.Entry{LinkID: link.ID, OriginalURL: link.OriginalURL, ExpiresAt: link.ExpiresAt}

	hits, err := s.cache.Hit(ctx, code, s.opts.HitWindow)
	if err != nil {
		s.log.Warn("cache hit counter failed", "err", err)
		return entry, nil
	}
	if hits >= s.opts.PopularThreshold {
		ttl := s.opts.CacheTTL
		if link.ExpiresAt != nil {
			if left := link.ExpiresAt.Sub(s.now()); left < ttl {
				ttl = left
			}
		}
		if ttl > 0 {
			if err := s.cache.Set(ctx, code, *entry, ttl); err != nil {
				s.log.Warn("cache set failed", "err", err)
			}
		}
	}
	return entry, nil
}

// CheckActive проверяет, что ссылка существует, активна и не истекла.
// В отличие от Resolve, переход не учитывается в статистике и счётчик
// популярности не меняется: метод используется для выдачи QR-кода.
func (s *LinkService) CheckActive(ctx context.Context, code string) error {
	if !shortcode.LooksValid(code) {
		return ErrNotFound
	}
	now := s.now()
	if entry, err := s.cache.Get(ctx, code); err == nil {
		if entry.ExpiresAt != nil && !now.Before(*entry.ExpiresAt) {
			return ErrGone
		}
		return nil
	} else if !errors.Is(err, cache.ErrMiss) {
		s.log.Warn("cache get failed", "err", err)
	}
	link, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if !link.IsActive || link.Expired(now) {
		return ErrGone
	}
	return nil
}

// List возвращает ссылки владельца.
func (s *LinkService) List(ctx context.Context, ownerID int64, limit, offset int) ([]model.Link, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return s.store.ListByOwner(ctx, ownerID, limit, offset)
}

// Get возвращает ссылку владельца по коду.
func (s *LinkService) Get(ctx context.Context, ownerID int64, code string) (*model.Link, error) {
	link, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if link.OwnerID != ownerID || !link.IsActive {
		return nil, ErrNotFound
	}
	return link, nil
}

// Delete деактивирует ссылку и убирает её из кэша.
func (s *LinkService) Delete(ctx context.Context, ownerID int64, code string) error {
	if err := s.store.Deactivate(ctx, ownerID, code); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		s.log.Warn("cache delete failed", "code", code, "err", err)
	}
	return nil
}

// Stats возвращает статистику по ссылке за [from, to).
func (s *LinkService) Stats(ctx context.Context, ownerID int64, code string, from, to time.Time) (*model.LinkStats, error) {
	if !from.Before(to) || to.Sub(from) > maxStatsPeriod {
		return nil, ErrBadPeriod
	}
	link, err := s.Get(ctx, ownerID, code)
	if err != nil {
		return nil, err
	}
	st, err := s.store.Stats(ctx, link.ID, from, to, s.opts.StatsTopN)
	if err != nil {
		return nil, err
	}
	st.Code = code
	st.From, st.To = from, to
	return st, nil
}

// TopLinks возвращает самые популярные коды по данным кэша.
func (s *LinkService) TopLinks(ctx context.Context, n int) ([]string, error) {
	return s.cache.TopCodes(ctx, n)
}
