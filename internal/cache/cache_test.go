package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

var (
	_ LinkCache = (*MemoryCache)(nil)
	_ LinkCache = (*RedisCache)(nil)
)

// harness — реализация кэша плюс способ «перемотать» время.
type harness struct {
	c       LinkCache
	advance func(d time.Duration)
}

func newMemoryHarness(t *testing.T) harness {
	m := NewMemory()
	cur := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return cur }
	return harness{c: m, advance: func(d time.Duration) { cur = cur.Add(d) }}
}

func newRedisHarness(t *testing.T) harness {
	mr := miniredis.RunT(t)
	cl := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	c := NewRedisFromClient(cl)
	t.Cleanup(func() { _ = c.Close() })
	return harness{c: c, advance: mr.FastForward}
}

// runSuite — общий набор тестов для всех реализаций LinkCache.
func runSuite(t *testing.T, mk func(*testing.T) harness) {
	ctx := context.Background()

	t.Run("GetMiss", func(t *testing.T) {
		h := mk(t)
		if _, err := h.c.Get(ctx, "nope"); !errors.Is(err, ErrMiss) {
			t.Fatalf("ожидался ErrMiss, получено %v", err)
		}
	})

	t.Run("SetGet", func(t *testing.T) {
		h := mk(t)
		exp := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
		want := Entry{LinkID: 42, OriginalURL: "https://example.com/a", ExpiresAt: &exp}
		if err := h.c.Set(ctx, "abc", want, time.Minute); err != nil {
			t.Fatal(err)
		}
		got, err := h.c.Get(ctx, "abc")
		if err != nil {
			t.Fatal(err)
		}
		if got.LinkID != want.LinkID || got.OriginalURL != want.OriginalURL ||
			got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
			t.Fatalf("получено %+v, ожидалось %+v", got, want)
		}
	})

	t.Run("TTLExpiry", func(t *testing.T) {
		h := mk(t)
		if err := h.c.Set(ctx, "abc", Entry{LinkID: 1, OriginalURL: "https://x.io"}, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		h.advance(5 * time.Second)
		if _, err := h.c.Get(ctx, "abc"); err != nil {
			t.Fatalf("запись должна ещё жить: %v", err)
		}
		h.advance(6 * time.Second)
		if _, err := h.c.Get(ctx, "abc"); !errors.Is(err, ErrMiss) {
			t.Fatalf("ожидался ErrMiss после TTL, получено %v", err)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		h := mk(t)
		_ = h.c.Set(ctx, "abc", Entry{LinkID: 1, OriginalURL: "https://x.io"}, time.Minute)
		if err := h.c.Delete(ctx, "abc"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.Get(ctx, "abc"); !errors.Is(err, ErrMiss) {
			t.Fatalf("ожидался ErrMiss, получено %v", err)
		}
		if err := h.c.Delete(ctx, "abc"); err != nil {
			t.Fatalf("повторное удаление не должно падать: %v", err)
		}
	})

	t.Run("HitWindow", func(t *testing.T) {
		h := mk(t)
		win := 10 * time.Second
		for i := int64(1); i <= 3; i++ {
			n, err := h.c.Hit(ctx, "abc", win)
			if err != nil {
				t.Fatal(err)
			}
			if n != i {
				t.Fatalf("Hit #%d вернул %d", i, n)
			}
		}
		h.advance(11 * time.Second)
		n, err := h.c.Hit(ctx, "abc", win)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("после окна ожидалось 1, получено %d", n)
		}
	})

	t.Run("TopCodes", func(t *testing.T) {
		h := mk(t)
		hits := map[string]int{"a": 1, "b": 5, "c": 3}
		for code, k := range hits {
			for i := 0; i < k; i++ {
				if _, err := h.c.Hit(ctx, code, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
		}
		got, err := h.c.TopCodes(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "b" || got[1] != "c" {
			t.Fatalf("получено %v, ожидалось [b c]", got)
		}
		all, _ := h.c.TopCodes(ctx, 10)
		if len(all) != 3 || all[2] != "a" {
			t.Fatalf("получено %v, ожидалось [b c a]", all)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		h := mk(t)
		if err := h.c.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMemoryCache(t *testing.T) { runSuite(t, newMemoryHarness) }
func TestRedisCache(t *testing.T)  { runSuite(t, newRedisHarness) }
