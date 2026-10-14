package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/service"
)

// benchTracker — заглушка трекера: не копит события.
type benchTracker struct{}

func (benchTracker) Track(analytics.Event) {}

// benchHandler собирает сервер на фейках из server_test.go и создаёт одну ссылку.
// withCache=true — ссылка лежит в кэше памяти (быстрый путь), иначе fakeCache
// всегда промахивается и каждый запрос идёт в хранилище.
func benchHandler(b *testing.B, withCache bool) (http.Handler, string) {
	b.Helper()
	store := newFakeStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var lc cache.LinkCache = fakeCache{}
	if withCache {
		lc = cache.NewMemory()
	}
	link := &model.Link{Code: "bench01", OriginalURL: "https://example.com/some/long/path?x=1", OwnerID: 1, IsActive: true}
	if err := store.Create(context.Background(), link); err != nil {
		b.Fatal(err)
	}
	if withCache {
		e := cache.Entry{LinkID: link.ID, OriginalURL: link.OriginalURL}
		if err := lc.Set(context.Background(), link.Code, e, time.Hour); err != nil {
			b.Fatal(err)
		}
	}

	svc := service.New(store, lc, &seqGen{}, benchTracker{}, service.DefaultOptions(), log)
	h := NewServer(Deps{
		Links:   svc,
		Auth:    &fakeAuth{},
		Metrics: NewMetrics(),
		BaseURL: "http://sho.rt",
		Logger:  log,
	})
	return h, link.Code
}

func benchRedirect(b *testing.B, withCache, parallel bool) {
	h, code := benchHandler(b, withCache)
	do := func() {
		req := httptest.NewRequest(http.MethodGet, "/"+code, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (bench)")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			b.Errorf("статус %d, ожидался 302", rec.Code)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	if !parallel {
		for i := 0; i < b.N; i++ {
			do()
		}
		return
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			do()
		}
	})
}

// Полный стек middleware + маршрутизатор + сервис, без сети (ResponseRecorder).
func BenchmarkRedirectHandlerCacheHit(b *testing.B)         { benchRedirect(b, true, false) }
func BenchmarkRedirectHandlerCacheHitParallel(b *testing.B) { benchRedirect(b, true, true) }
func BenchmarkRedirectHandlerCacheMiss(b *testing.B)        { benchRedirect(b, false, false) }

// BenchmarkRedirectHTTPLoopback — настоящий HTTP через loopback (httptest.Server),
// клиент не следует за редиректами.
func BenchmarkRedirectHTTPLoopback(b *testing.B) {
	h, code := benchHandler(b, true)
	ts := httptest.NewServer(h)
	defer ts.Close()

	client := &http.Client{
		Transport:     &http.Transport{MaxIdleConnsPerHost: 64},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	url := ts.URL + "/" + code
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				b.Errorf("статус %d", resp.StatusCode)
				return
			}
		}
	})
}
