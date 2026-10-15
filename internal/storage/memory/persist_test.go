package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func closeT(t *testing.T, p *Persistent) {
	t.Helper()
	if err := p.Close(); err != nil {
		t.Error(err)
	}
}

func openT(t *testing.T, path string, debounce time.Duration) *Persistent {
	t.Helper()
	p, err := Open(path, debounce, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Данные переживают «перезапуск»: Close -> Open того же файла.
func TestPersistentRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.json")
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	day := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	p := openT(t, path, time.Hour) // большая задержка: сохраняет только Close
	u := &model.User{Email: "Owner@Example.com", PasswordHash: "$2a$12$hash"}
	if err := p.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	keep := &model.Link{Code: "keep", OriginalURL: "https://a.example", OwnerID: u.ID, Title: "Заголовок", ExpiresAt: &exp, IsActive: true}
	gone := &model.Link{Code: "gone", OriginalURL: "https://b.example", OwnerID: u.ID, IsActive: true}
	for _, l := range []*model.Link{keep, gone} {
		if err := p.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	clicks := []model.Click{
		{LinkID: keep.ID, ClickedAt: day, Country: "RU", Referrer: "t.me", Browser: "Chrome", OS: "Windows", Device: "desktop", VisitorID: "v1"},
		{LinkID: keep.ID, ClickedAt: day, Country: "KZ", Referrer: "direct", Browser: "Safari", OS: "iOS", Device: "mobile", VisitorID: "v2"},
		{LinkID: keep.ID, ClickedAt: day, Country: "RU", Referrer: "t.me", Browser: "Chrome", OS: "Windows", Device: "desktop", VisitorID: "v1"},
	}
	if err := p.InsertBatch(ctx, clicks); err != nil {
		t.Fatal(err)
	}
	if err := p.AddClicks(ctx, map[int64]int64{keep.ID: 3}); err != nil {
		t.Fatal(err)
	}
	if err := p.Deactivate(ctx, u.ID, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := openT(t, path, time.Hour)
	defer closeT(t, p2)

	// Пользователь — вместе с хэшем пароля (в API он скрыт, но в файле обязан быть).
	gotU, err := p2.GetByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if gotU.ID != u.ID || gotU.PasswordHash != "$2a$12$hash" || !gotU.CreatedAt.Equal(u.CreatedAt) {
		t.Errorf("пользователь после загрузки: %+v", gotU)
	}

	gotL, err := p2.GetByCode(ctx, "keep")
	if err != nil {
		t.Fatal(err)
	}
	if gotL.Title != "Заголовок" || gotL.ExpiresAt == nil || !gotL.ExpiresAt.Equal(exp) || gotL.ClickCount != 3 || !gotL.IsActive {
		t.Errorf("ссылка после загрузки: %+v", gotL)
	}
	if g, _ := p2.GetByCode(ctx, "gone"); g == nil || g.IsActive {
		t.Errorf("отключённая ссылка должна остаться отключённой: %+v", g)
	}

	// Статистика восстановлена, включая уникальных посетителей (VisitorID тоже скрыт от JSON API).
	st, err := p2.Stats(ctx, keep.ID, day.Add(-time.Hour), day.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalClicks != 3 || st.UniqueVisitors != 2 || st.ByCountry[0].Key != "RU" {
		t.Errorf("статистика после загрузки: total=%d unique=%d countries=%v", st.TotalClicks, st.UniqueVisitors, st.ByCountry)
	}

	// Новые ID продолжают нумерацию, а занятые коды и email по-прежнему заняты.
	next := &model.Link{Code: "next", OriginalURL: "https://c.example", OwnerID: u.ID, IsActive: true}
	if err := p2.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	if next.ID != gone.ID+1 {
		t.Errorf("ID новой ссылки %d, ожидался %d", next.ID, gone.ID+1)
	}
	if err := p2.Create(ctx, &model.Link{Code: "keep"}); !errors.Is(err, storage.ErrConflict) {
		t.Errorf("код keep должен быть занят, получено %v", err)
	}
	if err := p2.CreateUser(ctx, &model.User{Email: "OWNER@example.com"}); !errors.Is(err, storage.ErrConflict) {
		t.Errorf("email должен быть занят, получено %v", err)
	}
}

// Изменения сохраняются в фоне, даже если процесс не дошёл до Close
// (например, окно консоли закрыли крестиком).
func TestPersistentAutosave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	p := openT(t, path, 20*time.Millisecond)
	defer closeT(t, p)

	if err := p.CreateUser(context.Background(), &model.User{Email: "auto@example.com", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "auto@example.com") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("данные не сохранились в фоне за 3 с (err=%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Временные файлы не остаются.
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(tmp) != 0 {
		t.Errorf("остались временные файлы: %v", tmp)
	}
}

// Без изменений файл не создаётся и не перезаписывается.
func TestPersistentNoChangesNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	p := openT(t, path, time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("файл создан без изменений данных: %v", err)
	}
	// Повторный Close безопасен.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// Повреждённый файл не перезаписывается: Open возвращает ошибку, содержимое не трогается.
func TestPersistentCorruptedFile(t *testing.T) {
	tests := []struct{ name, content, wantErr string }{
		{"не JSON", "{not json", "повреждён"},
		{"чужой формат", `{"format": 99}`, "формат"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, time.Millisecond, quietLog); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ожидалась ошибка с %q, получено %v", tc.wantErr, err)
			}
			if data, _ := os.ReadFile(path); string(data) != tc.content {
				t.Fatal("повреждённый файл был изменён")
			}
		})
	}
}

// Параллельные изменения и сохранения не теряют данных.
func TestPersistentConcurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.json")
	p := openT(t, path, time.Millisecond)
	if err := p.CreateUser(ctx, &model.User{Email: "c@example.com"}); err != nil {
		t.Fatal(err)
	}
	l := &model.Link{Code: "conc", OriginalURL: "https://x.example", OwnerID: 1, IsActive: true}
	if err := p.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = p.InsertBatch(ctx, []model.Click{{LinkID: l.ID, ClickedAt: time.Now(), VisitorID: "v"}})
				_ = p.AddClicks(ctx, map[int64]int64{l.ID: 1})
				if i%10 == 0 {
					_ = p.Save()
				}
			}
		}()
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := openT(t, path, time.Hour)
	defer closeT(t, p2)
	got, err := p2.GetByCode(ctx, "conc")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p2.Stats(ctx, l.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 1)
	if got.ClickCount != 400 || st.TotalClicks != 400 {
		t.Fatalf("после загрузки click_count=%d, переходов=%d, ожидалось 400", got.ClickCount, st.TotalClicks)
	}
}

// Ошибка записи (каталога нет) возвращается из Save, а не теряется молча.
func TestPersistentSaveError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "data.json")
	p := openT(t, path, time.Hour)
	if err := p.CreateUser(context.Background(), &model.User{Email: "e@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil {
		t.Fatal("ожидалась ошибка записи в несуществующий каталог")
	}
}
