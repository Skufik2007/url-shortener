package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage/memory"
)

// baseTime — фиксированный «текущий момент» для подмены s.now.
var baseTime = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// fakeTracker собирает события в срез под мьютексом.
type fakeTracker struct {
	mu     sync.Mutex
	events []analytics.Event
}

func (f *fakeTracker) Track(e analytics.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeTracker) snapshot() []analytics.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]analytics.Event(nil), f.events...)
}

// seqGen выдаёт заранее заданную последовательность кодов.
type seqGen struct {
	mu    sync.Mutex
	codes []string
	calls int
	err   error
}

func (g *seqGen) Generate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return "", g.err
	}
	if g.calls >= len(g.codes) {
		return "", errors.New("seqGen: коды закончились")
	}
	c := g.codes[g.calls]
	g.calls++
	return c, nil
}

// flakyCache — обёртка над LinkCache: может возвращать ошибки и запоминает TTL.
type flakyCache struct {
	cache.LinkCache
	failGet, failHit, failSet, failDelete bool
	lastTTL                               time.Duration
	sets                                  int
}

var errCache = errors.New("cache unavailable")

func (c *flakyCache) Get(ctx context.Context, code string) (*cache.Entry, error) {
	if c.failGet {
		return nil, errCache
	}
	return c.LinkCache.Get(ctx, code)
}

func (c *flakyCache) Hit(ctx context.Context, code string, w time.Duration) (int64, error) {
	if c.failHit {
		return 0, errCache
	}
	return c.LinkCache.Hit(ctx, code, w)
}

func (c *flakyCache) Set(ctx context.Context, code string, e cache.Entry, ttl time.Duration) error {
	if c.failSet {
		return errCache
	}
	c.sets++
	c.lastTTL = ttl
	return c.LinkCache.Set(ctx, code, e, ttl)
}

func (c *flakyCache) Delete(ctx context.Context, code string) error {
	if c.failDelete {
		return errCache
	}
	return c.LinkCache.Delete(ctx, code)
}

// errStore — хранилище, у которого часть операций падает «прочей» ошибкой.
type errStore struct{ *memory.Store }

var errStoreBoom = errors.New("db down")

func (errStore) GetByCode(context.Context, string) (*model.Link, error) { return nil, errStoreBoom }
func (errStore) Deactivate(context.Context, int64, string) error        { return errStoreBoom }
func (errStore) Create(context.Context, *model.Link) error              { return errStoreBoom }

// statsFailStore — хранилище, у которого падает только Stats.
type statsFailStore struct{ *memory.Store }

func (statsFailStore) Stats(context.Context, int64, time.Time, time.Time, int) (*model.LinkStats, error) {
	return nil, errStoreBoom
}

// countingStore считает обращения к GetByCode.
type countingStore struct {
	*memory.Store
	getCalls int
}

func (b *countingStore) GetByCode(ctx context.Context, code string) (*model.Link, error) {
	b.getCalls++
	return b.Store.GetByCode(ctx, code)
}

type env struct {
	svc     *LinkService
	store   *memory.Store
	cache   *flakyCache
	tracker *fakeTracker
	gen     *seqGen
	now     time.Time
}

func newEnv(t *testing.T, codes ...string) *env {
	t.Helper()
	e := &env{
		store:   memory.New(),
		cache:   &flakyCache{LinkCache: cache.NewMemory()},
		tracker: &fakeTracker{},
		gen:     &seqGen{codes: codes},
		now:     baseTime,
	}
	opts := DefaultOptions()
	opts.PublicHost = "short.example"
	e.svc = New(e.store, e.cache, e.gen, e.tracker, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.svc.now = func() time.Time { return e.now }
	return e
}

// createLink создаёт ссылку с алиасом через сервис.
func (e *env) createLink(t *testing.T, owner int64, alias string, exp *time.Time) *model.Link {
	t.Helper()
	l, err := e.svc.Create(context.Background(), owner, CreateInput{URL: "http://example.com/" + alias, Alias: alias, ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func timePtr(t time.Time) *time.Time        { return &t }
func durPtr(d time.Duration) *time.Duration { return &d }

func TestNew_NilLogger(t *testing.T) {
	s := New(memory.New(), cache.NewMemory(), &seqGen{}, &fakeTracker{}, DefaultOptions(), nil)
	if s.log == nil {
		t.Fatal("логгер по умолчанию не установлен")
	}
}

func TestCreate(t *testing.T) {
	ctx := context.Background()
	past := baseTime.Add(-time.Hour)
	future := baseTime.Add(time.Hour)

	tests := []struct {
		name    string
		in      CreateInput
		wantURL string
		wantErr error
	}{
		{"нормализация схемы и хоста", CreateInput{URL: "  HTTPS://Example.COM:8080/Path?q=1  "}, "https://example.com:8080/Path?q=1", nil},
		{"простой http", CreateInput{URL: "http://example.org"}, "http://example.org", nil},
		{"пустой", CreateInput{URL: "   "}, "", ErrInvalidURL},
		{"слишком длинный", CreateInput{URL: "http://example.com/" + strings.Repeat("a", maxURLLength)}, "", ErrInvalidURL},
		{"ftp", CreateInput{URL: "ftp://example.com/file"}, "", ErrInvalidURL},
		{"javascript", CreateInput{URL: "javascript:alert(1)"}, "", ErrInvalidURL},
		{"без хоста", CreateInput{URL: "http:///path"}, "", ErrInvalidURL},
		{"нераспознаваемый URL", CreateInput{URL: "http://exa mple.com/%zz"}, "", ErrInvalidURL},
		{"PublicHost без порта", CreateInput{URL: "http://short.example/abc"}, "", ErrInvalidURL},
		{"PublicHost с портом", CreateInput{URL: "https://SHORT.example:8443/abc"}, "", ErrInvalidURL},
		{"expires_at в прошлом", CreateInput{URL: "http://example.com", ExpiresAt: &past}, "", ErrInvalidURL},
		{"expires_at сейчас", CreateInput{URL: "http://example.com", ExpiresAt: timePtr(baseTime)}, "", ErrInvalidURL},
		{"expires_at в будущем", CreateInput{URL: "http://example.com", ExpiresAt: &future}, "http://example.com", nil},
		{"валидный алиас", CreateInput{URL: "http://example.com", Alias: "my-link_1"}, "http://example.com", nil},
		{"короткий алиас", CreateInput{URL: "http://example.com", Alias: "ab"}, "", ErrBadAlias},
		{"алиас с недопустимыми символами", CreateInput{URL: "http://example.com", Alias: "a b!c"}, "", ErrBadAlias},
		{"зарезервированный алиас", CreateInput{URL: "http://example.com", Alias: "admin"}, "", ErrBadAlias},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "gen0001")
			l, err := e.svc.Create(ctx, 1, tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ожидалась ошибка %v, получено %v", tc.wantErr, err)
				}
				if l != nil {
					t.Fatalf("при ошибке ссылка должна быть nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if l.OriginalURL != tc.wantURL {
				t.Errorf("URL = %q, ожидался %q", l.OriginalURL, tc.wantURL)
			}
			if !l.IsActive || l.OwnerID != 1 || l.ID == 0 {
				t.Errorf("некорректная ссылка: %+v", l)
			}
			wantCode := "gen0001"
			if tc.in.Alias != "" {
				wantCode = tc.in.Alias
			}
			if l.Code != wantCode {
				t.Errorf("код = %q, ожидался %q", l.Code, wantCode)
			}
		})
	}
}

func TestCreate_AliasTaken(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://a.com", Alias: "taken"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Create(ctx, 2, CreateInput{URL: "http://b.com", Alias: "taken"})
	if !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("ожидалась ErrAliasTaken, получено %v", err)
	}
}

func TestCreate_Collisions(t *testing.T) {
	ctx := context.Background()

	t.Run("повтор со следующим кодом", func(t *testing.T) {
		e := newEnv(t, "busy001", "free001")
		if _, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://a.com", Alias: "busy001"}); err != nil {
			t.Fatal(err)
		}
		l, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://b.com"})
		if err != nil {
			t.Fatal(err)
		}
		if l.Code != "free001" {
			t.Errorf("код = %q, ожидался free001", l.Code)
		}
		if e.gen.calls != 2 {
			t.Errorf("вызовов генератора = %d, ожидалось 2", e.gen.calls)
		}
	})

	t.Run("5 коллизий подряд", func(t *testing.T) {
		e := newEnv(t, "dup", "dup", "dup", "dup", "dup", "never")
		if _, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://a.com", Alias: "dup"}); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://b.com"})
		if err == nil || !strings.Contains(err.Error(), "unique") {
			t.Fatalf("ожидалась ошибка уникальности, получено %v", err)
		}
		if e.gen.calls != generateRetries {
			t.Errorf("вызовов генератора = %d, ожидалось %d", e.gen.calls, generateRetries)
		}
	})

	t.Run("ошибка генератора", func(t *testing.T) {
		e := newEnv(t)
		e.gen.err = errors.New("rng broken")
		if _, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://a.com"}); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})
}

func TestCreate_StoreError(t *testing.T) {
	e := newEnv(t, "abc1234")
	e.svc.store = errStore{e.store}
	for _, in := range []CreateInput{
		{URL: "http://a.com", Alias: "someal"},
		{URL: "http://a.com"},
	} {
		if _, err := e.svc.Create(context.Background(), 1, in); !errors.Is(err, errStoreBoom) {
			t.Errorf("ожидалась ошибка хранилища, получено %v", err)
		}
	}
}

func TestCreate_TitleTruncate(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		title    string
		wantRune int
	}{
		{"короткий с пробелами", "  hello  ", 5},
		{"ровно 200", strings.Repeat("я", 200), 200},
		{"длиннее 200 рун", strings.Repeat("я", 300), 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "titl001")
			l, err := e.svc.Create(ctx, 1, CreateInput{URL: "http://a.com", Title: tc.title})
			if err != nil {
				t.Fatal(err)
			}
			if got := len([]rune(l.Title)); got != tc.wantRune {
				t.Errorf("длина title = %d рун, ожидалось %d", got, tc.wantRune)
			}
		})
	}
}

func TestHostOnly(t *testing.T) {
	tests := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"example.com:8080", "example.com"},
		{"localhost:8080", "localhost"},
		{"[::1]:8080", "::1"},
		{"[::1]", "::1"},
	}
	for _, tc := range tests {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

func TestResolve_Errors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "dead001", nil)
	if err := e.svc.Delete(ctx, 1, "dead001"); err != nil {
		t.Fatal(err)
	}
	e.createLink(t, 1, "exp0001", timePtr(baseTime.Add(time.Hour)))

	tests := []struct {
		name    string
		code    string
		advance time.Duration
		wantErr error
	}{
		{"несуществующий", "nothere", 0, ErrNotFound},
		{"невалидный с косой чертой", "a/b", 0, ErrNotFound},
		{"слишком длинный", strings.Repeat("a", 40), 0, ErrNotFound},
		{"пустой", "", 0, ErrNotFound},
		{"деактивированный", "dead001", 0, ErrGone},
		{"просроченный", "exp0001", 2 * time.Hour, ErrGone},
		{"ровно в момент истечения", "exp0001", time.Hour, ErrGone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.now = baseTime.Add(tc.advance)
			_, err := e.svc.Resolve(ctx, tc.code, Visit{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ожидалась %v, получено %v", tc.wantErr, err)
			}
		})
	}
	if n := len(e.tracker.snapshot()); n != 0 {
		t.Errorf("событий в трекере = %d, ожидалось 0", n)
	}
}

func TestResolve_InvalidCodeSkipsStore(t *testing.T) {
	cs := &countingStore{Store: memory.New()}
	s := New(cs, cache.NewMemory(), &seqGen{}, &fakeTracker{}, DefaultOptions(), nil)
	for _, code := range []string{"a/b", strings.Repeat("x", 40)} {
		if _, err := s.Resolve(context.Background(), code, Visit{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("код %q: ожидалась ErrNotFound, получено %v", code, err)
		}
	}
	if cs.getCalls != 0 {
		t.Errorf("хранилище вызвано %d раз, ожидалось 0", cs.getCalls)
	}
}

func TestStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.store = errStore{e.store}
	if _, err := e.svc.Resolve(ctx, "abc", Visit{}); !errors.Is(err, errStoreBoom) {
		t.Errorf("Resolve: ожидалась ошибка хранилища, получено %v", err)
	}
	if _, err := e.svc.Get(ctx, 1, "abc"); !errors.Is(err, errStoreBoom) {
		t.Errorf("Get: ожидалась ошибка хранилища, получено %v", err)
	}
	if err := e.svc.Delete(ctx, 1, "abc"); !errors.Is(err, errStoreBoom) {
		t.Errorf("Delete: ожидалась ошибка хранилища, получено %v", err)
	}
}

func TestResolve_SuccessTracksVisit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.createLink(t, 1, "visit01", nil)
	v := Visit{IP: "1.2.3.4", UserAgent: "UA", Referer: "http://ref", CountryHint: "DE"}
	url, err := e.svc.Resolve(ctx, "visit01", v)
	if err != nil {
		t.Fatal(err)
	}
	if url != l.OriginalURL {
		t.Errorf("url = %q, ожидался %q", url, l.OriginalURL)
	}
	evs := e.tracker.snapshot()
	if len(evs) != 1 {
		t.Fatalf("событий = %d, ожидалось 1", len(evs))
	}
	want := analytics.Event{LinkID: l.ID, Time: baseTime, IP: "1.2.3.4", UserAgent: "UA", Referer: "http://ref", CountryHint: "DE"}
	if evs[0] != want {
		t.Errorf("событие = %+v, ожидалось %+v", evs[0], want)
	}
}

func TestResolve_CacheOnlyWhenPopular(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "pop0001", nil)

	// PopularThreshold = 3: после 1-го и 2-го обращений в кэше пусто.
	for i := 1; i <= 2; i++ {
		if _, err := e.svc.Resolve(ctx, "pop0001", Visit{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.cache.Get(ctx, "pop0001"); !errors.Is(err, cache.ErrMiss) {
			t.Fatalf("после %d обращений ссылка уже в кэше (err=%v)", i, err)
		}
	}
	if _, err := e.svc.Resolve(ctx, "pop0001", Visit{}); err != nil {
		t.Fatal(err)
	}
	ent, err := e.cache.Get(ctx, "pop0001")
	if err != nil {
		t.Fatalf("после 3 обращений ссылка должна быть в кэше: %v", err)
	}
	if ent.OriginalURL != "http://example.com/pop0001" {
		t.Errorf("в кэше неверный URL: %q", ent.OriginalURL)
	}
	// Четвёртое обращение идёт из кэша и тоже учитывается.
	if _, err := e.svc.Resolve(ctx, "pop0001", Visit{}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.tracker.snapshot()); n != 4 {
		t.Errorf("событий = %d, ожидалось 4", n)
	}
}

func TestResolve_CacheTTL(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		life    *time.Duration // срок жизни ссылки от baseTime
		wantTTL time.Duration
	}{
		{"без срока — полный CacheTTL", nil, time.Hour},
		{"срок больше CacheTTL", durPtr(5 * time.Hour), time.Hour},
		{"срок меньше CacheTTL", durPtr(10 * time.Minute), 10 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			var exp *time.Time
			if tc.life != nil {
				exp = timePtr(baseTime.Add(*tc.life))
			}
			e.createLink(t, 1, "ttl0001", exp)
			for i := 0; i < 3; i++ {
				if _, err := e.svc.Resolve(ctx, "ttl0001", Visit{}); err != nil {
					t.Fatal(err)
				}
			}
			if e.cache.sets != 1 {
				t.Fatalf("Set вызван %d раз, ожидался 1", e.cache.sets)
			}
			if e.cache.lastTTL != tc.wantTTL {
				t.Errorf("TTL = %v, ожидался %v", e.cache.lastTTL, tc.wantTTL)
			}
			if tc.life != nil && e.cache.lastTTL > *tc.life {
				t.Errorf("TTL %v превышает остаток жизни ссылки %v", e.cache.lastTTL, *tc.life)
			}
		})
	}
}

func TestResolve_DegradedCache(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(c *flakyCache)
	}{
		{"Get падает", func(c *flakyCache) { c.failGet = true }},
		{"Hit падает", func(c *flakyCache) { c.failHit = true }},
		{"Set падает", func(c *flakyCache) { c.failSet = true }},
		{"всё падает", func(c *flakyCache) { c.failGet, c.failHit, c.failSet = true, true, true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.createLink(t, 1, "deg0001", nil)
			tc.setup(e.cache)
			for i := 0; i < 4; i++ {
				url, err := e.svc.Resolve(ctx, "deg0001", Visit{})
				if err != nil {
					t.Fatalf("обращение %d: %v", i, err)
				}
				if url != l.OriginalURL {
					t.Fatalf("url = %q", url)
				}
			}
			if n := len(e.tracker.snapshot()); n != 4 {
				t.Errorf("событий = %d, ожидалось 4", n)
			}
		})
	}
}

func TestResolve_CachedEntryExpired(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	exp := baseTime.Add(time.Minute)
	if err := e.cache.Set(ctx, "cexp001", cache.Entry{LinkID: 1, OriginalURL: "http://x.com", ExpiresAt: &exp}, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Пока срок не вышел — редирект работает из кэша.
	if url, err := e.svc.Resolve(ctx, "cexp001", Visit{}); err != nil || url != "http://x.com" {
		t.Fatalf("url=%q err=%v", url, err)
	}
	e.now = baseTime.Add(2 * time.Minute)
	if _, err := e.svc.Resolve(ctx, "cexp001", Visit{}); !errors.Is(err, ErrGone) {
		t.Fatalf("ожидалась ErrGone, получено %v", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for i := 0; i < 120; i++ {
		l := &model.Link{Code: "code" + strconv.Itoa(i), OriginalURL: "http://a.com", OwnerID: 1, IsActive: true}
		if err := e.store.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name          string
		limit, offset int
		want          int
	}{
		{"лимит по умолчанию (0)", 0, 0, 50},
		{"отрицательный лимит", -5, 0, 50},
		{"лимит больше максимума", 1000, 0, 50},
		{"лимит 100 допустим", 100, 0, 100},
		{"малый лимит", 7, 0, 7},
		{"отрицательный offset", 10, -3, 10},
		{"offset с остатком", 100, 100, 20},
		{"offset за пределами", 10, 500, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.svc.List(ctx, 1, tc.limit, tc.offset)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != tc.want {
				t.Errorf("получено %d, ожидалось %d", len(res), tc.want)
			}
		})
	}
	if res, _ := e.svc.List(ctx, 2, 10, 0); len(res) != 0 {
		t.Errorf("у другого владельца ссылок быть не должно, получено %d", len(res))
	}
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "get0001", nil)
	e.createLink(t, 1, "get0002", nil)
	if err := e.svc.Delete(ctx, 1, "get0002"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		owner   int64
		code    string
		wantErr error
	}{
		{"свой", 1, "get0001", nil},
		{"чужой", 2, "get0001", ErrNotFound},
		{"несуществующий", 1, "nope", ErrNotFound},
		{"деактивированный", 1, "get0002", ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := e.svc.Get(ctx, tc.owner, tc.code)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ошибка = %v, ожидалась %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && l.Code != tc.code {
				t.Errorf("код = %q", l.Code)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "del0001", nil)
	if err := e.cache.Set(ctx, "del0001", cache.Entry{LinkID: 1, OriginalURL: "http://x"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.Delete(ctx, 2, "del0001"); !errors.Is(err, ErrNotFound) {
		t.Errorf("чужая: ожидалась ErrNotFound, получено %v", err)
	}
	if _, err := e.cache.Get(ctx, "del0001"); err != nil {
		t.Errorf("чужое удаление не должно трогать кэш: %v", err)
	}
	if err := e.svc.Delete(ctx, 1, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("несуществующая: ожидалась ErrNotFound, получено %v", err)
	}
	if err := e.svc.Delete(ctx, 1, "del0001"); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if _, err := e.cache.Get(ctx, "del0001"); !errors.Is(err, cache.ErrMiss) {
		t.Errorf("запись должна быть удалена из кэша, err=%v", err)
	}
	if _, err := e.svc.Resolve(ctx, "del0001", Visit{}); !errors.Is(err, ErrGone) {
		t.Errorf("Resolve после удаления: ожидалась ErrGone, получено %v", err)
	}
}

func TestDelete_Twice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "twice01", nil)
	if err := e.svc.Delete(ctx, 1, "twice01"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, 1, "twice01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление: ожидалась ErrNotFound, получено %v", err)
	}
}

func TestDelete_CacheErrorIgnored(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "dce0001", nil)
	e.cache.failDelete = true
	if err := e.svc.Delete(ctx, 1, "dce0001"); err != nil {
		t.Fatalf("ошибка кэша не должна ломать удаление: %v", err)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.createLink(t, 1, "stat001", nil)

	from := baseTime.Add(-24 * time.Hour)
	to := baseTime.Add(24 * time.Hour)
	clicks := []model.Click{
		{LinkID: l.ID, ClickedAt: baseTime, VisitorID: "v1", Country: "DE", Referrer: "direct", Browser: "Chrome", OS: "Windows", Device: "desktop"},
		{LinkID: l.ID, ClickedAt: baseTime.Add(time.Minute), VisitorID: "v1", Country: "DE", Referrer: "direct", Browser: "Chrome", OS: "Windows", Device: "desktop"},
		{LinkID: l.ID, ClickedAt: baseTime.Add(2 * time.Minute), VisitorID: "v2", Country: "FR", Referrer: "t.co", Browser: "Firefox", OS: "Linux", Device: "mobile"},
		{LinkID: l.ID, ClickedAt: from.Add(-time.Hour), VisitorID: "v3"}, // вне периода
	}
	if err := e.store.InsertBatch(ctx, clicks); err != nil {
		t.Fatal(err)
	}

	st, err := e.svc.Stats(ctx, 1, "stat001", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalClicks != 3 || st.UniqueVisitors != 2 {
		t.Errorf("Total=%d Unique=%d, ожидалось 3 и 2", st.TotalClicks, st.UniqueVisitors)
	}
	if st.Code != "stat001" || !st.From.Equal(from) || !st.To.Equal(to) {
		t.Errorf("Code/From/To некорректны: %+v", st)
	}
	if len(st.ByCountry) != 2 || st.ByCountry[0].Key != "DE" || st.ByCountry[0].Count != 2 {
		t.Errorf("ByCountry = %+v", st.ByCountry)
	}
}

func TestStats_Errors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "sterr01", nil)

	tests := []struct {
		name     string
		owner    int64
		code     string
		from, to time.Time
		wantErr  error
	}{
		{"from == to", 1, "sterr01", baseTime, baseTime, ErrBadPeriod},
		{"from > to", 1, "sterr01", baseTime.Add(time.Hour), baseTime, ErrBadPeriod},
		{"период больше года", 1, "sterr01", baseTime, baseTime.Add(maxStatsPeriod + time.Second), ErrBadPeriod},
		{"чужая ссылка", 2, "sterr01", baseTime, baseTime.Add(time.Hour), ErrNotFound},
		{"несуществующая", 1, "nothere", baseTime, baseTime.Add(time.Hour), ErrNotFound},
		{"ровно максимальный период", 1, "sterr01", baseTime, baseTime.Add(maxStatsPeriod), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.Stats(ctx, tc.owner, tc.code, tc.from, tc.to)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ошибка = %v, ожидалась %v", err, tc.wantErr)
			}
		})
	}
}

func TestStats_StoreError(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "ste0001", nil)
	e.svc.store = statsFailStore{e.store}
	if _, err := e.svc.Stats(ctx, 1, "ste0001", baseTime, baseTime.Add(time.Hour)); !errors.Is(err, errStoreBoom) {
		t.Fatalf("ожидалась ошибка хранилища, получено %v", err)
	}
}

func TestTopLinks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "top0001", nil)
	e.createLink(t, 1, "top0002", nil)
	for _, c := range []string{"top0001", "top0002", "top0002"} {
		if _, err := e.svc.Resolve(ctx, c, Visit{}); err != nil {
			t.Fatal(err)
		}
	}
	top, err := e.svc.TopLinks(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0] != "top0002" {
		t.Errorf("TopLinks = %v", top)
	}
}

func TestCheckActive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.createLink(t, 1, "live001", nil)
	e.createLink(t, 1, "dead001", nil)
	if err := e.svc.Delete(ctx, 1, "dead001"); err != nil {
		t.Fatal(err)
	}
	e.createLink(t, 1, "exp0001", timePtr(baseTime.Add(time.Hour)))
	// Закэшированная ссылка, срок которой истечёт.
	e.createLink(t, 1, "hot0001", timePtr(baseTime.Add(2*time.Hour)))
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Resolve(ctx, "hot0001", Visit{}); err != nil {
			t.Fatal(err)
		}
	}
	eventsBefore := len(e.tracker.events)

	tests := []struct {
		name    string
		code    string
		shift   time.Duration
		failGet bool
		want    error
	}{
		{"активная ссылка", "live001", 0, false, nil},
		{"несуществующая", "nope123", 0, false, ErrNotFound},
		{"недопустимый код", "a/b", 0, false, ErrNotFound},
		{"удалённая", "dead001", 0, false, ErrGone},
		{"ещё не истекла", "exp0001", 30 * time.Minute, false, nil},
		{"истекла", "exp0001", time.Hour, false, ErrGone},
		{"из кэша, активная", "hot0001", 0, false, nil},
		{"из кэша, истекла", "hot0001", 3 * time.Hour, false, ErrGone},
		{"кэш недоступен — читаем из БД", "live001", 0, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.now = baseTime.Add(tc.shift)
			e.cache.failGet = tc.failGet
			defer func() { e.now, e.cache.failGet = baseTime, false }()
			if err := e.svc.CheckActive(ctx, tc.code); !errors.Is(err, tc.want) {
				t.Errorf("CheckActive(%q) = %v, ожидалось %v", tc.code, err, tc.want)
			}
		})
	}
	if len(e.tracker.events) != eventsBefore {
		t.Errorf("CheckActive не должен попадать в аналитику: было %d событий, стало %d", eventsBefore, len(e.tracker.events))
	}
}

func TestCheckActive_StoreError(t *testing.T) {
	s := New(errStore{memory.New()}, cache.NewMemory(), &seqGen{}, &fakeTracker{}, DefaultOptions(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.CheckActive(context.Background(), "abc1234"); !errors.Is(err, errStoreBoom) {
		t.Fatalf("ожидалась ошибка хранилища, получено %v", err)
	}
}
