package service

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/shortcode"
	"github.com/Skufik2007/url-shortener/internal/storage/memory"
)

// benchTracker — трекер-заглушка: не копит события, чтобы не искажать замеры.
type benchTracker struct{}

func (benchTracker) Track(analytics.Event) {}

// missCache всегда промахивается на Get и не кэширует: так Resolve каждый раз
// идёт в хранилище.
type missCache struct{ cache.LinkCache }

func (missCache) Get(context.Context, string) (*cache.Entry, error) { return nil, cache.ErrMiss }
func (missCache) Hit(context.Context, string, time.Duration) (int64, error) {
	return 0, nil
}

func benchService(b *testing.B, wrap func(cache.LinkCache) cache.LinkCache) (*LinkService, string) {
	b.Helper()
	store := memory.New()
	var lc cache.LinkCache = cache.NewMemory()
	if wrap != nil {
		lc = wrap(lc)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(store, lc, shortcode.NewRandom(7), benchTracker{}, DefaultOptions(), log)
	link, err := svc.Create(context.Background(), 1, CreateInput{URL: "https://example.com/some/long/path?x=1"})
	if err != nil {
		b.Fatal(err)
	}
	return svc, link.Code
}

var benchVisit = Visit{IP: "203.0.113.7", UserAgent: "Mozilla/5.0 (bench)", Referer: "https://example.org/"}

func warmCache(b *testing.B, svc *LinkService, code string) {
	b.Helper()
	// Популярной ссылка становится после PopularThreshold обращений.
	for i := 0; i < 5; i++ {
		if _, err := svc.Resolve(context.Background(), code, benchVisit); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolveCacheHit(b *testing.B) {
	svc, code := benchService(b, nil)
	warmCache(b, svc, code)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Resolve(ctx, code, benchVisit); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolveCacheHitParallel(b *testing.B) {
	svc, code := benchService(b, nil)
	warmCache(b, svc, code)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := svc.Resolve(ctx, code, benchVisit); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkResolveCacheMiss(b *testing.B) {
	svc, code := benchService(b, func(c cache.LinkCache) cache.LinkCache { return missCache{c} })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Resolve(ctx, code, benchVisit); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolveCacheMissParallel(b *testing.B) {
	svc, code := benchService(b, func(c cache.LinkCache) cache.LinkCache { return missCache{c} })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := svc.Resolve(ctx, code, benchVisit); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkCreateRandomCode(b *testing.B) {
	svc, _ := benchService(b, nil)
	ctx := context.Background()
	in := CreateInput{URL: "https://example.com/page", Title: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Create(ctx, 1, in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCreateRandomCodeParallel(b *testing.B) {
	svc, _ := benchService(b, nil)
	ctx := context.Background()
	in := CreateInput{URL: "https://example.com/page", Title: "bench"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := svc.Create(ctx, 1, in); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
