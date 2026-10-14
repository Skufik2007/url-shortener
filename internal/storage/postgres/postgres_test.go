//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUsersAndLinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	u := &model.User{Email: fmt.Sprintf("u%d@example.com", suffix), PasswordHash: "h"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID) })
	if err := s.CreateUser(ctx, &model.User{Email: u.Email}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("дубликат email: %v", err)
	}
	if got, err := s.GetByEmail(ctx, u.Email); err != nil || got.ID != u.ID {
		t.Fatalf("GetByEmail: %v %v", got, err)
	}
	if got, err := s.GetByID(ctx, u.ID); err != nil || got.Email != u.Email {
		t.Fatalf("GetByID: %v %v", got, err)
	}
	if _, err := s.GetByID(ctx, -1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetByID: %v", err)
	}

	code := fmt.Sprintf("t%d", suffix)
	l := &model.Link{Code: code, OriginalURL: "https://example.com", OwnerID: u.ID, IsActive: true}
	if err := s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, &model.Link{Code: code, OriginalURL: "x", OwnerID: u.ID}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("дубликат кода: %v", err)
	}
	if _, err := s.GetByCode(ctx, "no-such-"+code); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetByCode: %v", err)
	}

	if err := s.AddClicks(ctx, map[int64]int64{l.ID: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddClicks(ctx, map[int64]int64{l.ID: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByCode(ctx, code)
	if err != nil || got.ClickCount != 5 || got.OwnerID != u.ID {
		t.Fatalf("GetByCode: %+v %v", got, err)
	}

	list, err := s.ListByOwner(ctx, u.ID, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByOwner: %v %v", list, err)
	}
	if err := s.Deactivate(ctx, u.ID+1, code); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Deactivate чужой: %v", err)
	}
	if err := s.Deactivate(ctx, u.ID, code); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListByOwner(ctx, u.ID, 10, 0); len(list) != 0 {
		t.Fatalf("после Deactivate: %v", list)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	l := &model.Link{Code: fmt.Sprintf("s%d", time.Now().UnixNano()), OriginalURL: "https://example.com", IsActive: true}
	if err := s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM links WHERE id = $1`, l.ID) })

	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	mk := func(at time.Time, v, country, br string) model.Click {
		return model.Click{LinkID: l.ID, ClickedAt: at, VisitorID: v, Country: country,
			Referrer: "direct", Browser: br, OS: "Windows", Device: "desktop"}
	}
	err := s.InsertBatch(ctx, []model.Click{
		mk(d1, "v1", "US", "Chrome"),
		mk(d1.Add(time.Hour), "v1", "US", "Chrome"),
		mk(d1.AddDate(0, 0, 1), "v2", "DE", "Firefox"),
		mk(d1.AddDate(0, 0, 1), "v3", "FR", "Chrome"),
		mk(d1.AddDate(0, 0, 9), "v4", "US", "Safari"), // вне периода
	})
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st, err := s.Stats(ctx, l.ID, from, from.AddDate(0, 0, 3), 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Code != l.Code || st.TotalClicks != 4 || st.UniqueVisitors != 3 {
		t.Fatalf("итоги: %+v", st)
	}
	if len(st.ByDay) != 2 || st.ByDay[0].Count != 2 || st.ByDay[1].Count != 2 || !st.ByDay[0].Day.Equal(from) {
		t.Fatalf("ByDay: %+v", st.ByDay)
	}
	if len(st.ByCountry) != 2 || st.ByCountry[0].Key != "US" || st.ByCountry[0].Count != 2 || st.ByCountry[1].Key != "DE" {
		t.Fatalf("ByCountry: %+v", st.ByCountry)
	}
	if len(st.ByBrowser) != 2 || st.ByBrowser[0].Key != "Chrome" || st.ByBrowser[0].Count != 3 {
		t.Fatalf("ByBrowser: %+v", st.ByBrowser)
	}
}
