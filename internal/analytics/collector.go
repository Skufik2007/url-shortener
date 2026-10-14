package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

// Config — параметры сборщика.
type Config struct {
	Workers       int           // число воркеров
	Buffer        int           // ёмкость канала событий
	BatchSize     int           // размер пакета для записи
	FlushInterval time.Duration // максимальная задержка перед сбросом неполного пакета
	SelfHost      string        // собственный хост (для определения direct)
	Salt          string        // соль для VisitorID
}

// CollectorStats — счётчики работы сборщика.
type CollectorStats struct {
	Accepted uint64 // принято в канал
	Dropped  uint64 // отброшено при переполнении или после остановки
	Written  uint64 // записано в хранилище
	Failed   uint64 // не удалось записать
}

// Collector асинхронно обогащает и пакетно записывает переходы.
type Collector struct {
	cfg    Config
	links  storage.LinkRepository
	clicks storage.ClickRepository
	geo    GeoResolver
	log    *slog.Logger

	ch     chan Event
	mu     sync.RWMutex // защищает closed: Track под RLock, остановка под Lock
	closed bool

	accepted, dropped, written, failed atomic.Uint64
}

var _ Tracker = (*Collector)(nil)

// NewCollector создаёт сборщик; нулевые значения конфигурации заменяются разумными.
func NewCollector(cfg Config, links storage.LinkRepository, clicks storage.ClickRepository, geo GeoResolver, log *slog.Logger) *Collector {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.Buffer < 1 {
		cfg.Buffer = 1024
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if geo == nil {
		geo = NoopGeo{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Collector{cfg: cfg, links: links, clicks: clicks, geo: geo, log: log,
		ch: make(chan Event, cfg.Buffer)}
}

// Track неблокирующе ставит событие в очередь; при переполнении или после
// остановки событие отбрасывается и растёт счётчик Dropped.
func (c *Collector) Track(e Event) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		c.dropped.Add(1)
		return
	}
	select {
	case c.ch <- e:
		c.accepted.Add(1)
	default:
		c.dropped.Add(1)
	}
}

// Stats возвращает снимок счётчиков.
func (c *Collector) Stats() CollectorStats {
	return CollectorStats{
		Accepted: c.accepted.Load(),
		Dropped:  c.dropped.Load(),
		Written:  c.written.Load(),
		Failed:   c.failed.Load(),
	}
}

// Run запускает воркеров и блокируется до отмены ctx и полного дренажа канала.
func (c *Collector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}
	wg.Wait()
}

// stop закрывает приём новых событий (идемпотентно).
func (c *Collector) stop() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *Collector) worker(ctx context.Context) {
	batch := make([]model.Click, 0, c.cfg.BatchSize)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) > 0 {
			c.flush(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e := <-c.ch:
			batch = append(batch, c.enrich(e))
			if len(batch) >= c.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			c.stop()
			for { // дренаж: после stop новые события не поступают
				select {
				case e := <-c.ch:
					batch = append(batch, c.enrich(e))
					if len(batch) >= c.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// enrich превращает сырое событие в готовую запись о переходе.
func (c *Collector) enrich(e Event) model.Click {
	browser, osName, device := ParseUA(e.UserAgent)
	country := strings.ToUpper(strings.TrimSpace(e.CountryHint))
	if len(country) != 2 || !isLetters(country) || country == "XX" || country == "T1" {
		country = strings.ToUpper(strings.TrimSpace(c.geo.Country(e.IP)))
	}
	if country == "" {
		country = "ZZ"
	}
	t := e.Time
	if t.IsZero() {
		t = time.Now()
	}
	return model.Click{
		LinkID:    e.LinkID,
		ClickedAt: t,
		Referrer:  ReferrerDomain(e.Referer, c.cfg.SelfHost),
		Country:   country,
		Browser:   browser,
		OS:        osName,
		Device:    device,
		VisitorID: c.visitorID(e.IP, e.UserAgent),
	}
}

func isLetters(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// visitorID — первые 16 hex-символов SHA-256(соль + IP + "|" + UA).
func (c *Collector) visitorID(ip, ua string) string {
	sum := sha256.Sum256([]byte(c.cfg.Salt + ip + "|" + ua))
	return hex.EncodeToString(sum[:])[:16]
}

// flush записывает пакет и обновляет счётчики ссылок; ошибки только логируются.
func (c *Collector) flush(batch []model.Click) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.clicks.InsertBatch(ctx, batch); err != nil {
		c.failed.Add(uint64(len(batch)))
		c.log.Error("analytics: запись переходов не удалась", "err", err, "count", len(batch))
		return
	}
	c.written.Add(uint64(len(batch)))
	deltas := make(map[int64]int64)
	for _, cl := range batch {
		deltas[cl.LinkID]++
	}
	if err := c.links.AddClicks(ctx, deltas); err != nil {
		c.log.Error("analytics: обновление счётчиков не удалось", "err", err)
	}
}
