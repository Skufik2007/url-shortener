package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

var ctx = context.Background()

func TestLinkCreateAndGet(t *testing.T) {
	s := New()
	l := &model.Link{Code: "abc", OriginalURL: "https://example.com", OwnerID: 1, IsActive: true}
	if err := s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	if l.ID == 0 || l.CreatedAt.IsZero() {
		t.Fatalf("ID/CreatedAt не заполнены: %+v", l)
	}
	tests := []struct {
		name    string
		code    string
		wantErr error
	}{
		{"существует", "abc", nil},
		{"нет такого", "zzz", storage.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.GetByCode(ctx, tt.code)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got.ID != l.ID {
				t.Fatalf("ID = %d, want %d", got.ID, l.ID)
			}
		})
	}
	if err := s.Create(ctx, &model.Link{Code: "abc"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("дубликат кода: err = %v", err)
	}
}

func TestGetByCodeReturnsCopy(t *testing.T) {
	s := New()
	_ = s.Create(ctx, &model.Link{Code: "abc", IsActive: true})
	a, _ := s.GetByCode(ctx, "abc")
	a.IsActive = false
	b, _ := s.GetByCode(ctx, "abc")
	if !b.IsActive {
		t.Fatal("изменение копии повлияло на хранилище")
	}
}

func TestListByOwner(t *testing.T) {
	s := New()
	for i := 0; i < 5; i++ {
		_ = s.Create(ctx, &model.Link{Code: fmt.Sprintf("c%d", i), OwnerID: 1, IsActive: true})
	}
	_ = s.Create(ctx, &model.Link{Code: "other", OwnerID: 2, IsActive: true})
	if err := s.Deactivate(ctx, 1, "c0"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		owner         int64
		limit, offset int
		want          []string
	}{
		{"все активные, новые первыми", 1, 10, 0, []string{"c4", "c3", "c2", "c1"}},
		{"limit", 1, 2, 0, []string{"c4", "c3"}},
		{"offset", 1, 2, 2, []string{"c2", "c1"}},
		{"offset за пределами", 1, 10, 10, nil},
		{"чужой владелец", 2, 10, 0, []string{"other"}},
		{"нет ссылок", 3, 10, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListByOwner(ctx, tt.owner, tt.limit, tt.offset)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i, c := range tt.want {
				if got[i].Code != c {
					t.Errorf("[%d] = %s, want %s", i, got[i].Code, c)
				}
			}
		})
	}
}

func TestDeactivate(t *testing.T) {
	s := New()
	_ = s.Create(ctx, &model.Link{Code: "abc", OwnerID: 1, IsActive: true})
	tests := []struct {
		name    string
		owner   int64
		code    string
		wantErr error
	}{
		{"чужая ссылка", 2, "abc", storage.ErrNotFound},
		{"нет ссылки", 1, "nope", storage.ErrNotFound},
		{"своя ссылка", 1, "abc", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Deactivate(ctx, tt.owner, tt.code); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	l, _ := s.GetByCode(ctx, "abc")
	if l.IsActive {
		t.Fatal("ссылка осталась активной")
	}
}

func TestUsers(t *testing.T) {
	s := New()
	u := &model.User{Email: "A@x.com", PasswordHash: "h"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatal("ID/CreatedAt не заполнены")
	}
	if err := s.CreateUser(ctx, &model.User{Email: "a@X.com"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("дубликат email: %v", err)
	}
	if got, err := s.GetByEmail(ctx, "a@x.com"); err != nil || got.ID != u.ID {
		t.Fatalf("GetByEmail: %v %v", got, err)
	}
	if _, err := s.GetByEmail(ctx, "no@x.com"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got, err := s.GetByID(ctx, u.ID); err != nil || got.Email != "A@x.com" {
		t.Fatalf("GetByID: %v %v", got, err)
	}
	if _, err := s.GetByID(ctx, 999); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetByID: %v", err)
	}
}

func TestAddClicks(t *testing.T) {
	s := New()
	l := &model.Link{Code: "abc", IsActive: true}
	_ = s.Create(ctx, l)
	_ = s.AddClicks(ctx, map[int64]int64{l.ID: 3, 999: 5})
	_ = s.AddClicks(ctx, map[int64]int64{l.ID: 2})
	got, _ := s.GetByCode(ctx, "abc")
	if got.ClickCount != 5 {
		t.Fatalf("ClickCount = %d, want 5", got.ClickCount)
	}
}

func TestStats(t *testing.T) {
	s := New()
	l := &model.Link{Code: "abc", IsActive: true}
	_ = s.Create(ctx, l)
	d1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	d3 := d1.AddDate(0, 0, 5)
	mk := func(at time.Time, v, country, ref, br string) model.Click {
		return model.Click{LinkID: l.ID, ClickedAt: at, VisitorID: v, Country: country,
			Referrer: ref, Browser: br, OS: "Windows", Device: "desktop"}
	}
	clicks := []model.Click{
		mk(d2, "v1", "US", "direct", "Chrome"),
		mk(d1, "v1", "US", "direct", "Chrome"),
		mk(d1, "v2", "DE", "google.com", "Firefox"),
		mk(d1.Add(time.Hour), "v3", "FR", "habr.com", "Chrome"),
		mk(d3, "v4", "US", "direct", "Safari"),       // вне периода
		{LinkID: 777, ClickedAt: d1, VisitorID: "x"}, // другая ссылка
	}
	if err := s.InsertBatch(ctx, clicks); err != nil {
		t.Fatal(err)
	}
	from := d1.Truncate(24 * time.Hour)
	to := from.AddDate(0, 0, 3)

	st, err := s.Stats(ctx, l.ID, from, to, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Code != "abc" || st.TotalClicks != 4 || st.UniqueVisitors != 3 {
		t.Fatalf("итоги: %+v", st)
	}
	if len(st.ByDay) != 2 || st.ByDay[0].Count != 3 || st.ByDay[1].Count != 1 ||
		!st.ByDay[0].Day.Equal(from) || !st.ByDay[0].Day.Before(st.ByDay[1].Day) {
		t.Fatalf("ByDay: %+v", st.ByDay)
	}
	// topN=2: US(2), затем при равенстве DE/FR — по ключу, DE.
	wantC := []model.KeyCount{{Key: "US", Count: 2}, {Key: "DE", Count: 1}}
	if fmt.Sprint(st.ByCountry) != fmt.Sprint(wantC) {
		t.Fatalf("ByCountry: %+v", st.ByCountry)
	}
	if len(st.ByReferrer) != 2 || st.ByReferrer[0].Key != "direct" || st.ByReferrer[0].Count != 2 {
		t.Fatalf("ByReferrer: %+v", st.ByReferrer)
	}
	if len(st.ByBrowser) != 2 || st.ByBrowser[0].Key != "Chrome" || st.ByBrowser[0].Count != 3 {
		t.Fatalf("ByBrowser: %+v", st.ByBrowser)
	}
	if len(st.ByOS) != 1 || len(st.ByDevice) != 1 {
		t.Fatalf("ByOS/ByDevice: %+v %+v", st.ByOS, st.ByDevice)
	}

	// Пустой период и неизвестная ссылка — нули, не nil-срезы.
	empty, err := s.Stats(ctx, 12345, from, to, 5)
	if err != nil || empty.TotalClicks != 0 || empty.ByDay == nil || empty.ByCountry == nil {
		t.Fatalf("пустая статистика: %+v %v", empty, err)
	}
	// Граница to исключена.
	st, _ = s.Stats(ctx, l.ID, from, d2, 5)
	if st.TotalClicks != 3 {
		t.Fatalf("полуинтервал: %d", st.TotalClicks)
	}
}

func TestPingClose(t *testing.T) {
	s := New()
	if s.Ping(ctx) != nil || s.Close() != nil {
		t.Fatal("Ping/Close должны быть успешны")
	}
}

func TestConcurrent(t *testing.T) {
	s := New()
	l := &model.Link{Code: "abc", OwnerID: 1, IsActive: true}
	_ = s.Create(ctx, l)
	const workers, per = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = s.Create(ctx, &model.Link{Code: fmt.Sprintf("w%d-%d", w, i), OwnerID: 1, IsActive: true})
				_ = s.AddClicks(ctx, map[int64]int64{l.ID: 1})
				_ = s.InsertBatch(ctx, []model.Click{{LinkID: l.ID, ClickedAt: time.Now(), VisitorID: "v"}})
				_, _ = s.GetByCode(ctx, "abc")
				_, _ = s.ListByOwner(ctx, 1, 10, 0)
				_, _ = s.Stats(ctx, l.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 3)
			}
		}(w)
	}
	wg.Wait()
	got, _ := s.GetByCode(ctx, "abc")
	if got.ClickCount != workers*per {
		t.Fatalf("ClickCount = %d, want %d", got.ClickCount, workers*per)
	}
	all, _ := s.ListByOwner(ctx, 1, 10000, 0)
	if len(all) != workers*per+1 {
		t.Fatalf("ссылок %d, want %d", len(all), workers*per+1)
	}
}
