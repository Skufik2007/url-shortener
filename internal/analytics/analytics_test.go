package analytics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
)

func TestParseUA(t *testing.T) {
	cases := []struct {
		name, ua, browser, os, device string
	}{
		{"chrome win", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "Chrome", "Windows", "desktop"},
		{"edge win", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0", "Edge", "Windows", "desktop"},
		{"opera", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36 OPR/105.0.0.0", "Opera", "Windows", "desktop"},
		{"yandex", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 YaBrowser/23.11.0 Yowser/2.5 Safari/537.36", "Yandex", "Windows", "desktop"},
		{"firefox linux", "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0", "Firefox", "Linux", "desktop"},
		{"safari mac", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Safari/605.1.15", "Safari", "macOS", "desktop"},
		{"safari iphone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Mobile/15E148 Safari/604.1", "Safari", "iOS", "mobile"},
		{"safari ipad", "Mozilla/5.0 (iPad; CPU OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Mobile/15E148 Safari/604.1", "Safari", "iOS", "tablet"},
		{"chrome android", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36", "Chrome", "Android", "mobile"},
		{"android tablet", "Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "Chrome", "Android", "tablet"},
		{"samsung", "Mozilla/5.0 (Linux; Android 13; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Mobile Safari/537.36", "Samsung Internet", "Android", "mobile"},
		{"chromeos", "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", "Chrome", "ChromeOS", "desktop"},
		{"firefox ios", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/120.0 Mobile/15E148 Safari/605.1.15", "Firefox", "iOS", "mobile"},
		{"googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "Other", "Other", "bot"},
		{"curl", "curl/8.4.0", "Other", "Other", "bot"},
		{"go client", "Go-http-client/2.0", "Other", "Other", "bot"},
		{"python", "python-requests/2.31.0", "Other", "Other", "bot"},
		{"empty", "", "Other", "Other", "bot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, o, d := ParseUA(c.ua)
			if b != c.browser || o != c.os || d != c.device {
				t.Errorf("got (%s,%s,%s), want (%s,%s,%s)", b, o, d, c.browser, c.os, c.device)
			}
		})
	}
}

func TestReferrerDomain(t *testing.T) {
	cases := []struct{ ref, self, want string }{
		{"", "short.local", "direct"},
		{"::not a url", "short.local", "direct"},
		{"https://short.local/dashboard", "short.local", "direct"},
		{"https://www.Short.local/x", "short.local:8080", "direct"},
		{"http://localhost:8080/", "localhost:8080", "direct"},
		{"https://www.Google.com/search?q=x", "short.local", "google.com"},
		{"https://t.me/channel", "short.local", "t.me"},
		{"https://news.ycombinator.com:443/item", "short.local", "news.ycombinator.com"},
		{"android-app://com.google.android.gm", "short.local", "com.google.android.gm"},
		{"just-text", "short.local", "direct"},
	}
	for _, c := range cases {
		if got := ReferrerDomain(c.ref, c.self); got != c.want {
			t.Errorf("ReferrerDomain(%q,%q)=%q, want %q", c.ref, c.self, got, c.want)
		}
	}
}

type fakeGeo map[string]string

func (g fakeGeo) Country(ip string) string { return g[ip] }

// fakeRepo реализует ClickRepository и LinkRepository.
type fakeRepo struct {
	mu      sync.Mutex
	batches [][]model.Click
	deltas  map[int64]int64
	failIns bool
}

func (f *fakeRepo) InsertBatch(_ context.Context, c []model.Click) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIns {
		return errors.New("boom")
	}
	f.batches = append(f.batches, append([]model.Click(nil), c...))
	return nil
}

func (f *fakeRepo) AddClicks(_ context.Context, d map[int64]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deltas == nil {
		f.deltas = map[int64]int64{}
	}
	for k, v := range d {
		f.deltas[k] += v
	}
	return nil
}

func (f *fakeRepo) Stats(context.Context, int64, time.Time, time.Time, int) (*model.LinkStats, error) {
	return nil, nil
}
func (f *fakeRepo) Create(context.Context, *model.Link) error { return nil }
func (f *fakeRepo) GetByCode(context.Context, string) (*model.Link, error) {
	return nil, nil
}
func (f *fakeRepo) ListByOwner(context.Context, int64, int, int) ([]model.Link, error) {
	return nil, nil
}
func (f *fakeRepo) Deactivate(context.Context, int64, string) error { return nil }

func (f *fakeRepo) snapshot() (batches [][]model.Click, total int, deltas map[int64]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deltas = map[int64]int64{}
	for k, v := range f.deltas {
		deltas[k] = v
	}
	for _, b := range f.batches {
		total += len(b)
	}
	return append([][]model.Click(nil), f.batches...), total, deltas
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestCollector(cfg Config, repo *fakeRepo, geo GeoResolver) *Collector {
	return NewCollector(cfg, repo, repo, geo, quiet())
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("условие не выполнено за отведённое время")
}

func TestEnrich(t *testing.T) {
	c := newTestCollector(Config{SelfHost: "short.local", Salt: "s"}, &fakeRepo{}, fakeGeo{"8.8.8.8": "us"})
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	cl := c.enrich(Event{LinkID: 1, IP: "1.1.1.1", UserAgent: ua, Referer: "https://www.example.com/a", CountryHint: "de"})
	if cl.Country != "DE" || cl.Browser != "Chrome" || cl.OS != "Windows" || cl.Device != "desktop" || cl.Referrer != "example.com" {
		t.Errorf("неожиданный результат: %+v", cl)
	}
	if len(cl.VisitorID) != 16 || cl.VisitorID != c.visitorID("1.1.1.1", ua) {
		t.Errorf("VisitorID: %q", cl.VisitorID)
	}
	if c.visitorID("1.1.1.2", ua) == cl.VisitorID {
		t.Error("VisitorID должен зависеть от IP")
	}
	if got := c.enrich(Event{IP: "8.8.8.8"}).Country; got != "US" {
		t.Errorf("geo: %q", got)
	}
	if got := c.enrich(Event{IP: "10.0.0.1", CountryHint: "bad"}).Country; got != "ZZ" {
		t.Errorf("fallback: %q", got)
	}
	if got := c.enrich(Event{}).Referrer; got != "direct" {
		t.Errorf("referrer: %q", got)
	}
}

func TestCollectorBatchBySize(t *testing.T) {
	repo := &fakeRepo{}
	c := newTestCollector(Config{Workers: 1, Buffer: 100, BatchSize: 3, FlushInterval: time.Hour}, repo, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for i := 0; i < 6; i++ {
		c.Track(Event{LinkID: int64(i%2 + 1), Time: time.Now()})
	}
	waitFor(t, func() bool { _, n, _ := repo.snapshot(); return n == 6 })
	batches, _, deltas := repo.snapshot()
	if len(batches) != 2 || len(batches[0]) != 3 {
		t.Errorf("ожидалось 2 пакета по 3, получено %d", len(batches))
	}
	if deltas[1] != 3 || deltas[2] != 3 {
		t.Errorf("дельты: %v", deltas)
	}
	cancel()
	<-done
	if s := c.Stats(); s.Accepted != 6 || s.Written != 6 || s.Dropped != 0 || s.Failed != 0 {
		t.Errorf("stats: %+v", s)
	}
}

func TestCollectorFlushByTimer(t *testing.T) {
	repo := &fakeRepo{}
	c := newTestCollector(Config{Workers: 1, Buffer: 10, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, repo, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	c.Track(Event{LinkID: 1})
	waitFor(t, func() bool { _, n, _ := repo.snapshot(); return n == 1 })
	cancel()
	<-done
}

func TestCollectorDrainOnStop(t *testing.T) {
	repo := &fakeRepo{}
	c := newTestCollector(Config{Workers: 3, Buffer: 1000, BatchSize: 7, FlushInterval: time.Hour}, repo, nil)
	for i := 0; i < 100; i++ {
		c.Track(Event{LinkID: 5})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx) // вернётся только после дренажа
	_, n, deltas := repo.snapshot()
	if n != 100 || deltas[5] != 100 {
		t.Errorf("записано %d, дельта %d", n, deltas[5])
	}
	c.Track(Event{LinkID: 5}) // после остановки — отбрасывается
	if s := c.Stats(); s.Dropped != 1 || s.Written != 100 {
		t.Errorf("stats: %+v", s)
	}
}

func TestCollectorDropsOnOverflow(t *testing.T) {
	repo := &fakeRepo{}
	c := newTestCollector(Config{Workers: 1, Buffer: 2}, repo, nil)
	for i := 0; i < 5; i++ { // воркеры не запущены — канал заполняется
		c.Track(Event{LinkID: 1})
	}
	if s := c.Stats(); s.Accepted != 2 || s.Dropped != 3 {
		t.Errorf("stats: %+v", s)
	}
}

func TestCollectorWriteFailure(t *testing.T) {
	repo := &fakeRepo{failIns: true}
	c := newTestCollector(Config{Workers: 1, Buffer: 10, BatchSize: 2}, repo, nil)
	c.Track(Event{LinkID: 1})
	c.Track(Event{LinkID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
	if s := c.Stats(); s.Failed != 2 || s.Written != 0 {
		t.Errorf("stats: %+v", s)
	}
}
