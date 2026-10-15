package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

// snapshotFormat — версия формата файла; при несовместимых изменениях увеличивается.
const snapshotFormat = 1

// Структуры снимка отделены от моделей: в model поля PasswordHash и VisitorID
// скрыты от JSON (json:"-"), чтобы не попадать в ответы API, а в файле они нужны.
type snapshot struct {
	Format      int        `json:"format"`
	SavedAt     time.Time  `json:"saved_at"`
	NextLinkID  int64      `json:"next_link_id"`
	NextUserID  int64      `json:"next_user_id"`
	NextClickID int64      `json:"next_click_id"`
	Users       []userRec  `json:"users"`
	Links       []linkRec  `json:"links"`
	Clicks      []clickRec `json:"clicks"`
}

type userRec struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

type linkRec struct {
	ID          int64      `json:"id"`
	Code        string     `json:"code"`
	OriginalURL string     `json:"original_url"`
	OwnerID     int64      `json:"owner_id"`
	Title       string     `json:"title,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	IsActive    bool       `json:"is_active"`
	ClickCount  int64      `json:"click_count"`
}

type clickRec struct {
	ID        int64     `json:"id"`
	LinkID    int64     `json:"link_id"`
	ClickedAt time.Time `json:"clicked_at"`
	Referrer  string    `json:"referrer"`
	Country   string    `json:"country"`
	Browser   string    `json:"browser"`
	OS        string    `json:"os"`
	Device    string    `json:"device"`
	VisitorID string    `json:"visitor_id"`
}

// takeSnapshot копирует состояние хранилища под блокировкой на чтение
// и возвращает его вместе с номером версии.
func (s *Store) takeSnapshot() (snapshot, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := snapshot{
		Format:      snapshotFormat,
		NextLinkID:  s.nextLinkID,
		NextUserID:  s.nextUserID,
		NextClickID: s.nextClickID,
		Users:       make([]userRec, 0, len(s.users)),
		Links:       make([]linkRec, 0, len(s.links)),
	}
	for _, u := range s.users {
		snap.Users = append(snap.Users, userRec{u.ID, u.Email, u.PasswordHash, u.CreatedAt})
	}
	for _, l := range s.links {
		snap.Links = append(snap.Links, linkRec{l.ID, l.Code, l.OriginalURL, l.OwnerID, l.Title,
			l.CreatedAt, l.ExpiresAt, l.IsActive, l.ClickCount})
	}
	for _, cs := range s.clicks {
		for _, c := range cs {
			snap.Clicks = append(snap.Clicks, clickRec{c.ID, c.LinkID, c.ClickedAt, c.Referrer,
				c.Country, c.Browser, c.OS, c.Device, c.VisitorID})
		}
	}
	return snap, s.version
}

// restore заменяет содержимое пустого хранилища данными снимка.
func (s *Store) restore(snap snapshot) error {
	if snap.Format != snapshotFormat {
		return fmt.Errorf("неподдерживаемый формат снимка %d (ожидался %d)", snap.Format, snapshotFormat)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range snap.Users {
		s.users[u.ID] = &model.User{ID: u.ID, Email: u.Email, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt}
		s.userByMail[strings.ToLower(u.Email)] = u.ID
	}
	for _, l := range snap.Links {
		s.links[l.ID] = &model.Link{ID: l.ID, Code: l.Code, OriginalURL: l.OriginalURL, OwnerID: l.OwnerID,
			Title: l.Title, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, IsActive: l.IsActive, ClickCount: l.ClickCount}
		s.linkByCode[l.Code] = l.ID
	}
	for _, c := range snap.Clicks {
		s.clicks[c.LinkID] = append(s.clicks[c.LinkID], model.Click{ID: c.ID, LinkID: c.LinkID,
			ClickedAt: c.ClickedAt, Referrer: c.Referrer, Country: c.Country, Browser: c.Browser,
			OS: c.OS, Device: c.Device, VisitorID: c.VisitorID})
	}
	// Счётчики ID не меньше максимальных: защита от ручной правки файла.
	s.nextUserID, s.nextLinkID, s.nextClickID = snap.NextUserID, snap.NextLinkID, snap.NextClickID
	for id := range s.users {
		s.nextUserID = max(s.nextUserID, id)
	}
	for id := range s.links {
		s.nextLinkID = max(s.nextLinkID, id)
	}
	for _, c := range snap.Clicks {
		s.nextClickID = max(s.nextClickID, c.ID)
	}
	return nil
}

// Persistent — хранилище в памяти, которое сохраняет снимок своего состояния
// в JSON-файл: после изменений (с задержкой debounce) и при закрытии.
// Предназначено для демо-режима без PostgreSQL: данные переживают перезапуск.
type Persistent struct {
	*Store
	path     string
	debounce time.Duration
	log      *slog.Logger

	saveMu    sync.Mutex // сериализует записи файла
	savedVer  uint64     // версия данных, записанная последней (под saveMu)
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var _ storage.Store = (*Persistent)(nil)

// Open загружает снимок из path (если файла нет — начинает с пустого хранилища)
// и запускает фоновое сохранение. Повреждённый файл не перезаписывается:
// возвращается ошибка, чтобы не потерять данные.
func Open(path string, debounce time.Duration, log *slog.Logger) (*Persistent, error) {
	if log == nil {
		log = slog.Default()
	}
	st := New()
	path = filepath.Clean(path) // путь задаёт администратор через DATA_FILE
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Info("файл данных не найден, начинаем с пустого хранилища", "path", path)
	case err != nil:
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	default:
		var snap snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			return nil, fmt.Errorf("файл данных %s повреждён: %w", path, err)
		}
		if err := st.restore(snap); err != nil {
			return nil, fmt.Errorf("файл данных %s: %w", path, err)
		}
		log.Info("данные загружены из файла", "path", path,
			"users", len(snap.Users), "links", len(snap.Links), "clicks", len(snap.Clicks))
	}

	st.changed = make(chan struct{}, 1)
	p := &Persistent{
		Store:    st,
		path:     path,
		debounce: debounce,
		log:      log,
		savedVer: st.version,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go p.loop()
	return p, nil
}

// loop ждёт уведомления об изменениях и сохраняет снимок не чаще раза в debounce:
// поток переходов не превращается в поток записей на диск.
func (p *Persistent) loop() {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			return
		case <-p.changed:
		}
		select {
		case <-p.stop:
			return // финальное сохранение выполнит Close
		case <-time.After(p.debounce):
		}
		if err := p.Save(); err != nil {
			p.log.Error("сохранение данных в файл", "path", p.path, "err", err)
		}
	}
}

// Save записывает снимок в файл, если данные изменились с последней записи.
// Запись атомарная: во временный файл рядом, затем переименование поверх старого,
// поэтому при сбое посреди записи прежний файл остаётся целым.
func (p *Persistent) Save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	snap, ver := p.takeSnapshot()
	if ver == p.savedVer {
		return nil
	}
	snap.SavedAt = time.Now().UTC()
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(p.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // после успешного Rename файла уже нет
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		return err
	}
	p.savedVer = ver
	return nil
}

// Close останавливает фоновое сохранение и записывает итоговый снимок.
func (p *Persistent) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.done
		err = p.Save()
		if err == nil {
			p.log.Info("данные сохранены в файл", "path", p.path)
		}
	})
	return err
}
