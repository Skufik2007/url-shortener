// Package postgres содержит реализацию storage.Store на PostgreSQL (pgx/v5).
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Skufik2007/url-shortener/internal/model"
	"github.com/Skufik2007/url-shortener/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockID — ключ advisory-блокировки, исключающей параллельные миграции.
const migrationLockID int64 = 727_001

var _ storage.Store = (*Store)(nil)

// Store — хранилище на пуле соединений pgx.
type Store struct {
	pool *pgxpool.Pool
}

// New создаёт пул, проверяет соединение и применяет миграции.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: создание пула: %w", err)
	}
	s := &Store{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Migrate применяет ещё не применённые встроенные миграции по порядку номеров.
// Всё выполняется в одной транзакции под advisory-блокировкой.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: чтение миграций: %w", err)
	}
	type migration struct {
		version int
		name    string
	}
	var list []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("postgres: некорректное имя миграции %q", e.Name())
		}
		list = append(list, migration{v, e.Name()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: начало транзакции миграций: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // после Commit вернёт ErrTxClosed

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("postgres: блокировка миграций: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("postgres: schema_migrations: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("postgres: чтение schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range list {
		if applied[m.version] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("postgres: миграция %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("postgres: отметка миграции %s: %w", m.name, err)
		}
	}
	return tx.Commit(ctx)
}

// mapErr переводит ошибки pgx в ошибки пакета storage.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return storage.ErrConflict
	}
	return err
}

const linkColumns = `id, code, original_url, COALESCE(owner_id, 0), title, created_at, expires_at, is_active, click_count`

type scanner interface {
	Scan(dest ...any) error
}

func scanLink(r scanner, l *model.Link) error {
	return r.Scan(&l.ID, &l.Code, &l.OriginalURL, &l.OwnerID, &l.Title,
		&l.CreatedAt, &l.ExpiresAt, &l.IsActive, &l.ClickCount)
}

// Create сохраняет ссылку. ErrConflict, если код занят.
func (s *Store) Create(ctx context.Context, l *model.Link) error {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO links (code, original_url, owner_id, title, expires_at, is_active, click_count)
		 VALUES ($1, $2, NULLIF($3::bigint, 0), $4, $5, $6, $7)
		 RETURNING id, created_at`,
		l.Code, l.OriginalURL, l.OwnerID, l.Title, l.ExpiresAt, l.IsActive, l.ClickCount,
	).Scan(&l.ID, &l.CreatedAt)
	return mapErr(err)
}

// GetByCode возвращает ссылку по коду (включая неактивные).
func (s *Store) GetByCode(ctx context.Context, code string) (*model.Link, error) {
	var l model.Link
	row := s.pool.QueryRow(ctx, `SELECT `+linkColumns+` FROM links WHERE code = $1`, code)
	if err := scanLink(row, &l); err != nil {
		return nil, mapErr(err)
	}
	return &l, nil
}

// ListByOwner возвращает активные ссылки владельца, новые первыми.
func (s *Store) ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]model.Link, error) {
	if offset < 0 {
		offset = 0
	}
	var lim *int // nil -> LIMIT NULL, то есть без ограничения
	if limit >= 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+linkColumns+` FROM links
		 WHERE owner_id = $1 AND is_active
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2 OFFSET $3`, ownerID, lim, offset)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	res := []model.Link{}
	for rows.Next() {
		var l model.Link
		if err := scanLink(rows, &l); err != nil {
			return nil, err
		}
		res = append(res, l)
	}
	return res, rows.Err()
}

// Deactivate помечает ссылку неактивной. ErrNotFound, если ссылки нет или она чужая.
func (s *Store) Deactivate(ctx context.Context, ownerID int64, code string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE links SET is_active = false WHERE code = $1 AND owner_id = $2 AND is_active`, code, ownerID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// AddClicks увеличивает счётчики переходов одним запросом.
func (s *Store) AddClicks(ctx context.Context, deltas map[int64]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(deltas))
	vals := make([]int64, 0, len(deltas))
	for id, d := range deltas {
		ids = append(ids, id)
		vals = append(vals, d)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE links SET click_count = click_count + d.delta
		 FROM unnest($1::bigint[], $2::bigint[]) AS d(id, delta)
		 WHERE links.id = d.id`, ids, vals)
	return mapErr(err)
}

// CreateUser сохраняет пользователя. ErrConflict, если email занят.
func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, created_at`,
		u.Email, u.PasswordHash,
	).Scan(&u.ID, &u.CreatedAt)
	return mapErr(err)
}

// GetByEmail возвращает пользователя по email.
func (s *Store) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	var u model.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

// GetByID возвращает пользователя по ID.
func (s *Store) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

// InsertBatch пакетно сохраняет переходы через COPY.
func (s *Store) InsertBatch(ctx context.Context, clicks []model.Click) error {
	if len(clicks) == 0 {
		return nil
	}
	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"clicks"},
		[]string{"link_id", "clicked_at", "referrer", "country", "browser", "os", "device", "visitor_id"},
		pgx.CopyFromSlice(len(clicks), func(i int) ([]any, error) {
			c := clicks[i]
			return []any{c.LinkID, c.ClickedAt, c.Referrer, c.Country, c.Browser, c.OS, c.Device, c.VisitorID}, nil
		}))
	return mapErr(err)
}

// Запросы разрезов: каждый — готовая константа, без сборки строк.
const (
	qByCountry  = `SELECT trim(country) AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
	qByReferrer = `SELECT referrer AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
	qByBrowser  = `SELECT browser AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
	qByOS       = `SELECT os AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
	qByDevice   = `SELECT device AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
)

func (s *Store) topKeys(ctx context.Context, q string, linkID int64, from, to time.Time, topN int) ([]model.KeyCount, error) {
	if topN < 0 {
		topN = 0
	}
	rows, err := s.pool.Query(ctx, q, linkID, from, to, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := []model.KeyCount{}
	for rows.Next() {
		var kc model.KeyCount
		if err := rows.Scan(&kc.Key, &kc.Count); err != nil {
			return nil, err
		}
		res = append(res, kc)
	}
	return res, rows.Err()
}

// Stats считает агрегаты по ссылке за полуинтервал [from, to).
func (s *Store) Stats(ctx context.Context, linkID int64, from, to time.Time, topN int) (*model.LinkStats, error) {
	st := &model.LinkStats{From: from, To: to}

	// Код ссылки (если ссылки нет — пустой).
	err := s.pool.QueryRow(ctx, `SELECT code FROM links WHERE id = $1`, linkID).Scan(&st.Code)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	err = s.pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT visitor_id) FROM clicks
		 WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3`,
		linkID, from, to).Scan(&st.TotalClicks, &st.UniqueVisitors)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT date_trunc('day', clicked_at AT TIME ZONE 'UTC') AS d, count(*)
		 FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3
		 GROUP BY d ORDER BY d`, linkID, from, to)
	if err != nil {
		return nil, err
	}
	st.ByDay = []model.DayCount{}
	for rows.Next() {
		var dc model.DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		dc.Day = dc.Day.UTC()
		st.ByDay = append(st.ByDay, dc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, d := range []struct {
		q   string
		dst *[]model.KeyCount
	}{
		{qByCountry, &st.ByCountry},
		{qByReferrer, &st.ByReferrer},
		{qByBrowser, &st.ByBrowser},
		{qByOS, &st.ByOS},
		{qByDevice, &st.ByDevice},
	} {
		if *d.dst, err = s.topKeys(ctx, d.q, linkID, from, to, topN); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Ping проверяет соединение с базой.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close закрывает пул соединений.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}
