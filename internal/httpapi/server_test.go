package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/auth"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/service"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

// ---- фейки ----

type fakeStore struct {
	mu     sync.Mutex
	nextID int64
	links  map[string]*model.Link
	stats  *model.LinkStats
	// последние аргументы Stats
	lastFrom, lastTo time.Time
}

func newFakeStore() *fakeStore { return &fakeStore{links: map[string]*model.Link{}} }

func (s *fakeStore) Create(_ context.Context, l *model.Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[l.Code]; ok {
		return storage.ErrConflict
	}
	s.nextID++
	l.ID = s.nextID
	l.CreatedAt = time.Now()
	c := *l
	s.links[l.Code] = &c
	return nil
}

func (s *fakeStore) GetByCode(_ context.Context, code string) (*model.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok {
		return nil, storage.ErrNotFound
	}
	c := *l
	return &c, nil
}

func (s *fakeStore) ListByOwner(_ context.Context, owner int64, limit, offset int) ([]model.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Link
	for _, l := range s.links {
		if l.OwnerID == owner && l.IsActive {
			out = append(out, *l)
		}
	}
	return out, nil
}

func (s *fakeStore) Deactivate(_ context.Context, owner int64, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[code]
	if !ok || l.OwnerID != owner || !l.IsActive {
		return storage.ErrNotFound
	}
	l.IsActive = false
	return nil
}

func (s *fakeStore) AddClicks(context.Context, map[int64]int64) error { return nil }

func (s *fakeStore) CreateUser(context.Context, *model.User) error { return nil }
func (s *fakeStore) GetByEmail(context.Context, string) (*model.User, error) {
	return nil, storage.ErrNotFound
}
func (s *fakeStore) GetByID(context.Context, int64) (*model.User, error) {
	return nil, storage.ErrNotFound
}
func (s *fakeStore) InsertBatch(context.Context, []model.Click) error { return nil }

func (s *fakeStore) Stats(_ context.Context, linkID int64, from, to time.Time, _ int) (*model.LinkStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFrom, s.lastTo = from, to
	if s.stats != nil {
		c := *s.stats
		return &c, nil
	}
	return &model.LinkStats{}, nil
}

func (s *fakeStore) Ping(context.Context) error { return nil }
func (s *fakeStore) Close() error               { return nil }

type fakeCache struct{}

func (fakeCache) Get(context.Context, string) (*cache.Entry, error) { return nil, cache.ErrMiss }
func (fakeCache) Set(context.Context, string, cache.Entry, time.Duration) error {
	return nil
}
func (fakeCache) Delete(context.Context, string) error                      { return nil }
func (fakeCache) Hit(context.Context, string, time.Duration) (int64, error) { return 1, nil }
func (fakeCache) TopCodes(context.Context, int) ([]string, error)           { return []string{"abc"}, nil }
func (fakeCache) Ping(context.Context) error                                { return nil }
func (fakeCache) Close() error                                              { return nil }

type fakeTracker struct {
	mu     sync.Mutex
	events []analytics.Event
}

func (t *fakeTracker) Track(e analytics.Event) {
	t.mu.Lock()
	t.events = append(t.events, e)
	t.mu.Unlock()
}

type seqGen struct {
	mu sync.Mutex
	n  int
}

func (g *seqGen) Generate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("gen%04d", g.n), nil
}

// fakeAuth: один пользователь на email, токен вида "tok-<id>".
type fakeAuth struct {
	mu    sync.Mutex
	users map[string]struct {
		u  *model.User
		pw string
	}
}

func (a *fakeAuth) Register(_ context.Context, email, pw string) (*model.User, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.Contains(email, "@") {
		return nil, "", auth.ErrInvalidEmail
	}
	if len(pw) < 8 {
		return nil, "", auth.ErrWeakPassword
	}
	if a.users == nil {
		a.users = map[string]struct {
			u  *model.User
			pw string
		}{}
	}
	if _, ok := a.users[email]; ok {
		return nil, "", auth.ErrEmailTaken
	}
	u := &model.User{ID: int64(len(a.users) + 1), Email: email}
	a.users[email] = struct {
		u  *model.User
		pw string
	}{u, pw}
	return u, fmt.Sprintf("tok-%d", u.ID), nil
}

func (a *fakeAuth) Login(_ context.Context, email, pw string) (*model.User, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.users[email]
	if !ok || e.pw != pw {
		return nil, "", auth.ErrInvalidCredentials
	}
	return e.u, fmt.Sprintf("tok-%d", e.u.ID), nil
}

func (a *fakeAuth) ParseToken(tok string) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(tok, "tok-%d", &id); err != nil || id <= 0 {
		return 0, auth.ErrInvalidToken
	}
	return id, nil
}

// ---- окружение ----

type env struct {
	srv     *httptest.Server
	store   *fakeStore
	tracker *fakeTracker
	metrics *Metrics
	health  *bool // true — зависимость недоступна
}

func newEnv(t *testing.T, rps float64, burst int) *env {
	t.Helper()
	store := newFakeStore()
	tracker := &fakeTracker{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := service.DefaultOptions()
	opts.PublicHost = "sho.rt"
	svc := service.New(store, fakeCache{}, &seqGen{}, tracker, opts, log)
	down := false
	m := NewMetrics()
	h := NewServer(Deps{
		Links: svc,
		Auth:  &fakeAuth{},
		Health: []HealthCheck{{Name: "db", Ping: func(context.Context) error {
			if down {
				return errors.New("down")
			}
			return nil
		}}},
		Metrics:        m,
		CollectorStats: func() CollectorStats { return CollectorStats{Accepted: 7} },
		BaseURL:        "http://sho.rt/",
		RateLimitRPS:   rps,
		RateLimitBurst: burst,
		Dashboard:      fstest.MapFS{"index.html": {Data: []byte("<html>dash</html>")}},
		Logger:         log,
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &env{srv: ts, store: store, tracker: tracker, metrics: m, health: &down}
}

// do выполняет запрос без следования редиректам.
func (e *env) do(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (e *env) register(t *testing.T, email string) string {
	t.Helper()
	resp, data := e.do(t, "POST", "/api/v1/auth/register", "", map[string]string{"email": email, "password": "secret123"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, data)
	}
	var out struct{ Token string }
	_ = json.Unmarshal(data, &out)
	return out.Token
}

func wantStatus(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("статус %d, ожидался %d; тело: %s", resp.StatusCode, want, body)
	}
}

// ---- тесты ----

func TestRegisterAndLogin(t *testing.T) {
	e := newEnv(t, 0, 0)

	resp, body := e.do(t, "POST", "/api/v1/auth/register", "", map[string]string{"email": "a@b.co", "password": "secret123"})
	wantStatus(t, resp, body, 201)
	var ar struct {
		Token string `json:"token"`
		User  struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	_ = json.Unmarshal(body, &ar)
	if ar.Token == "" || ar.User.Email != "a@b.co" {
		t.Fatalf("неожиданный ответ: %s", body)
	}

	resp, body = e.do(t, "POST", "/api/v1/auth/register", "", map[string]string{"email": "a@b.co", "password": "secret123"})
	wantStatus(t, resp, body, 409)
	resp, body = e.do(t, "POST", "/api/v1/auth/register", "", map[string]string{"email": "x@b.co", "password": "123"})
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "POST", "/api/v1/auth/register", "", "not json")
	wantStatus(t, resp, body, 400)

	resp, body = e.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": "a@b.co", "password": "secret123"})
	wantStatus(t, resp, body, 200)
	resp, body = e.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": "a@b.co", "password": "wrong-pass"})
	wantStatus(t, resp, body, 401)

	resp, body = e.do(t, "GET", "/api/v1/me", ar.Token, nil)
	wantStatus(t, resp, body, 200)
}

func TestCreateLink(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")

	// Валидная ссылка со случайным кодом.
	resp, body := e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com/page", "title": "T"})
	wantStatus(t, resp, body, 201)
	var lr struct {
		Code     string `json:"code"`
		ShortURL string `json:"short_url"`
		Original string `json:"original_url"`
	}
	_ = json.Unmarshal(body, &lr)
	if lr.Code == "" || lr.ShortURL != "http://sho.rt/"+lr.Code || lr.Original != "https://example.com/page" {
		t.Fatalf("неожиданный ответ: %s", body)
	}

	// Алиас, затем повтор — конфликт.
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "my-link"})
	wantStatus(t, resp, body, 201)
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "my-link"})
	wantStatus(t, resp, body, 409)

	// Невалидные данные.
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "ftp://example.com"})
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "not a url"})
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "api"})
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "http://sho.rt/abc"})
	wantStatus(t, resp, body, 400)

	var er map[string]string
	_ = json.Unmarshal(body, &er)
	if er["error"] == "" {
		t.Fatalf("ожидалось поле error: %s", body)
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")
	big := `{"url":"https://example.com/` + strings.Repeat("a", 70<<10) + `"}`
	resp, body := e.do(t, "POST", "/api/v1/links", tok, big)
	wantStatus(t, resp, body, 413)
}

func TestRedirect(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com/x?y=1", "alias": "go-x"})

	req, _ := http.NewRequest("GET", e.srv.URL+"/go-x", nil)
	req.Header.Set("User-Agent", "TestAgent/1.0")
	req.Header.Set("Referer", "https://news.example/a")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://example.com/x?y=1" {
		t.Fatalf("статус %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=0") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if len(e.tracker.events) != 1 || e.tracker.events[0].UserAgent != "TestAgent/1.0" ||
		e.tracker.events[0].Referer != "https://news.example/a" || e.tracker.events[0].IP == "" {
		t.Fatalf("события аналитики: %+v", e.tracker.events)
	}
	if e.metrics.Redirects.Load() != 1 {
		t.Errorf("счётчик редиректов = %d", e.metrics.Redirects.Load())
	}
}

func TestRedirectNotFoundAndGone(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")

	resp, body := e.do(t, "GET", "/nope123", "", nil)
	wantStatus(t, resp, body, 404)

	// Истёкшая ссылка: кладём напрямую в хранилище.
	past := time.Now().Add(-time.Hour)
	_ = e.store.Create(context.Background(), &model.Link{Code: "old", OriginalURL: "https://example.com", OwnerID: 1, IsActive: true, ExpiresAt: &past})
	resp, body = e.do(t, "GET", "/old", "", nil)
	wantStatus(t, resp, body, 410)

	// Удалённая ссылка.
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "del-me"})
	resp, body = e.do(t, "DELETE", "/api/v1/links/del-me", tok, nil)
	wantStatus(t, resp, body, 204)
	resp, body = e.do(t, "GET", "/del-me", "", nil)
	wantStatus(t, resp, body, 410)
	// Повторное удаление и удаление чужой ссылки — 404.
	resp, body = e.do(t, "DELETE", "/api/v1/links/del-me", tok, nil)
	wantStatus(t, resp, body, 404)
}

func TestListGetDeleteOwnership(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok1 := e.register(t, "a@b.co")
	tok2 := e.register(t, "c@d.co")
	e.do(t, "POST", "/api/v1/links", tok1, map[string]string{"url": "https://example.com/1", "alias": "one"})
	e.do(t, "POST", "/api/v1/links", tok1, map[string]string{"url": "https://example.com/2", "alias": "two"})

	resp, body := e.do(t, "GET", "/api/v1/links", tok1, nil)
	wantStatus(t, resp, body, 200)
	var list struct {
		Links []struct {
			Code     string `json:"code"`
			ShortURL string `json:"short_url"`
		} `json:"links"`
	}
	_ = json.Unmarshal(body, &list)
	if len(list.Links) != 2 || list.Links[0].ShortURL == "" {
		t.Fatalf("список: %s", body)
	}

	resp, body = e.do(t, "GET", "/api/v1/links", tok2, nil)
	wantStatus(t, resp, body, 200)
	_ = json.Unmarshal(body, &list)
	if len(list.Links) != 0 {
		t.Fatalf("чужие ссылки в списке: %s", body)
	}
	resp, body = e.do(t, "GET", "/api/v1/links?limit=abc", tok1, nil)
	wantStatus(t, resp, body, 400)

	resp, body = e.do(t, "GET", "/api/v1/links/one", tok1, nil)
	wantStatus(t, resp, body, 200)
	resp, body = e.do(t, "GET", "/api/v1/links/one", tok2, nil)
	wantStatus(t, resp, body, 404)
	resp, body = e.do(t, "DELETE", "/api/v1/links/one", tok2, nil)
	wantStatus(t, resp, body, 404)
}

func TestStats(t *testing.T) {
	e := newEnv(t, 0, 0)
	tok := e.register(t, "a@b.co")
	e.do(t, "POST", "/api/v1/links", tok, map[string]string{"url": "https://example.com", "alias": "stat1"})
	e.store.stats = &model.LinkStats{TotalClicks: 5, UniqueVisitors: 3}

	resp, body := e.do(t, "GET", "/api/v1/links/stat1/stats?from=2026-01-01&to=2026-01-31", tok, nil)
	wantStatus(t, resp, body, 200)
	var st model.LinkStats
	_ = json.Unmarshal(body, &st)
	if st.TotalClicks != 5 || st.Code != "stat1" {
		t.Fatalf("статистика: %s", body)
	}
	wantFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) // to включительно (+24ч)
	if !e.store.lastFrom.Equal(wantFrom) || !e.store.lastTo.Equal(wantTo) {
		t.Errorf("период [%v, %v), ожидался [%v, %v)", e.store.lastFrom, e.store.lastTo, wantFrom, wantTo)
	}

	// По умолчанию — последние 30 дней, включая сегодня.
	resp, body = e.do(t, "GET", "/api/v1/links/stat1/stats", tok, nil)
	wantStatus(t, resp, body, 200)
	if d := e.store.lastTo.Sub(e.store.lastFrom); d != 30*24*time.Hour {
		t.Errorf("период по умолчанию = %v", d)
	}

	resp, body = e.do(t, "GET", "/api/v1/links/stat1/stats?from=bad", tok, nil)
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "GET", "/api/v1/links/stat1/stats?from=2026-02-01&to=2026-01-01", tok, nil)
	wantStatus(t, resp, body, 400)
	resp, body = e.do(t, "GET", "/api/v1/links/missing/stats", tok, nil)
	wantStatus(t, resp, body, 404)
}

func TestUnauthorized(t *testing.T) {
	e := newEnv(t, 0, 0)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/links"},
		{"GET", "/api/v1/links"},
		{"GET", "/api/v1/links/x"},
		{"DELETE", "/api/v1/links/x"},
		{"GET", "/api/v1/links/x/stats"},
		{"GET", "/api/v1/me"},
	} {
		resp, body := e.do(t, c.method, c.path, "", nil)
		if resp.StatusCode != 401 {
			t.Errorf("%s %s: статус %d (%s)", c.method, c.path, resp.StatusCode, body)
		}
	}
	resp, body := e.do(t, "GET", "/api/v1/links", "garbage", nil)
	wantStatus(t, resp, body, 401)
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, 0.001, 3)
	var last *http.Response
	var codes []int
	for i := 0; i < 5; i++ {
		resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": "a@b.co", "password": "xxxxxxxx"})
		codes = append(codes, resp.StatusCode)
		last = resp
	}
	if codes[0] != 401 || codes[2] != 401 || codes[3] != 429 || codes[4] != 429 {
		t.Fatalf("коды ответов: %v", codes)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("нет заголовка Retry-After")
	}
	if e.metrics.RateLimited.Load() != 2 {
		t.Errorf("счётчик rate-limit = %d", e.metrics.RateLimited.Load())
	}
	// Редирект и healthz не лимитируются.
	resp, body := e.do(t, "GET", "/healthz", "", nil)
	wantStatus(t, resp, body, 200)
}

func TestRateLimiterBucket(t *testing.T) {
	l := NewRateLimiter(1, 2)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("первый запрос")
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("второй запрос")
	}
	if ok, wait := l.Allow("a"); ok || wait <= 0 {
		t.Fatalf("третий должен быть отклонён, wait=%v", wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("другой ключ независим")
	}
	now = now.Add(1100 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("токен должен пополниться")
	}
	// Очистка старых ключей.
	now = now.Add(10 * time.Minute)
	l.Allow("c")
	if n := l.size(); n != 1 {
		t.Fatalf("после очистки ключей: %d, ожидался 1", n)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	e := newEnv(t, 0, 0)
	resp, body := e.do(t, "GET", "/healthz", "", nil)
	wantStatus(t, resp, body, 200)
	*e.health = true
	resp, body = e.do(t, "GET", "/healthz", "", nil)
	wantStatus(t, resp, body, 503)

	resp, body = e.do(t, "GET", "/metrics", "", nil)
	wantStatus(t, resp, body, 200)
	for _, want := range []string{"shortener_http_requests_total", "shortener_analytics_accepted_total 7"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("в метриках нет %q:\n%s", want, body)
		}
	}
}

func TestDashboardAndHeaders(t *testing.T) {
	e := newEnv(t, 0, 0)
	resp, body := e.do(t, "GET", "/", "", nil)
	wantStatus(t, resp, body, 302)
	if resp.Header.Get("Location") != "/dashboard/" {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}
	resp, body = e.do(t, "GET", "/dashboard/", "", nil)
	wantStatus(t, resp, body, 200)
	if !strings.Contains(string(body), "dash") {
		t.Errorf("тело дашборда: %s", body)
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if resp.Header.Get(h) != want {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
	if resp.Header.Get("Referrer-Policy") == "" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Error("нет Referrer-Policy или CSP")
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("нет X-Request-Id")
	}
}

func TestTop(t *testing.T) {
	e := newEnv(t, 0, 0)
	resp, body := e.do(t, "GET", "/api/v1/top?n=5", "", nil)
	wantStatus(t, resp, body, 200)
	if !strings.Contains(string(body), `"abc"`) {
		t.Errorf("тело: %s", body)
	}
	resp, body = e.do(t, "GET", "/api/v1/top?n=0", "", nil)
	wantStatus(t, resp, body, 400)
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.2")
	if ip := clientIP(r, false); ip != "10.0.0.1" {
		t.Errorf("без доверия: %q", ip)
	}
	if ip := clientIP(r, true); ip != "203.0.113.5" {
		t.Errorf("с доверием: %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "garbage")
	if ip := clientIP(r, true); ip != "10.0.0.1" {
		t.Errorf("мусор в XFF: %q", ip)
	}
}
