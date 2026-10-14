# 4 Реализация

## 4.1 Структура репозитория

Исходный код организован по стандартной для Go схеме: исполняемые команды находятся в `cmd/`, закрытые для импорта из других модулей пакеты — в `internal/`. Состав репозитория показан в листинге 4.1.

Листинг 4.1 — Структура каталогов репозитория

```
url-shortener/
├── cmd/
│   ├── shortener/main.go        точка входа сервиса, сборка зависимостей, graceful shutdown
│   └── loadgen/main.go          генератор нагрузки с перцентилями задержки
├── internal/
│   ├── analytics/               Collector, разбор User-Agent, Referer, GeoIP
│   ├── auth/                    регистрация, вход, bcrypt, HMAC-токены
│   ├── cache/                   интерфейс LinkCache, RedisCache, MemoryCache
│   ├── config/                  конфигурация из переменных окружения
│   ├── httpapi/                 маршруты, обработчики, middleware, rate limiting, метрики
│   ├── model/                   доменные сущности
│   ├── service/                 LinkService: создание, разрешение, статистика
│   ├── shortcode/               генератор кодов и проверка алиасов
│   └── storage/
│       ├── storage.go           интерфейсы репозиториев
│       ├── memory/              реализация в памяти
│       └── postgres/            реализация на pgx; migrations/001_init.sql
├── web/                         дашборд (index.html, app.js, style.css), embed.go
├── docs/                        проектный документ, сценарий k6, скриншоты, записка
├── .github/workflows/ci.yml     конвейер непрерывной интеграции
├── Dockerfile, docker-compose.yml, Makefile, .golangci.yml
└── go.mod, go.sum
```

Зависимости направлены строго от транспорта к данным: `httpapi` → `service` → (`storage`, `cache`, `analytics`). Пакеты `storage` и `cache` содержат интерфейсы и их реализации; сборка конкретных реализаций выполняется в `cmd/shortener/main.go`. Рабочий код на Go (включая генератор нагрузки и встраивание статики) занимает около 3,4 тыс. строк, тесты — около 2,8 тыс. строк.

## 4.2 Генератор коротких кодов

Генератор (`internal/shortcode/generator.go`) формирует код из алфавита base62 с помощью `crypto/rand`. Прямое взятие остатка от деления байта (0–255) на 62 приводило бы к смещению: 256 не кратно 62, и первые восемь символов алфавита выпадали бы чаще. Поэтому применено отсечение (rejection sampling): байты не меньше 248 (наибольшего кратного 62 числа, не превышающего 256) отбрасываются (листинг 4.2).

Листинг 4.2 — Генерация кода с отсечением смещённых значений

```go
const maxByte = 256 - 256%len(alphabet) // 248

func (r *Random) Generate() (string, error) {
	out := make([]byte, 0, r.length)
	buf := make([]byte, r.length+r.length/2+4)
	for len(out) < r.length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("shortcode: crypto/rand: %w", err)
		}
		for _, b := range buf {
			if int(b) >= maxByte {
				continue // отбрасываем, иначе первые символы алфавита выпадали бы чаще
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == r.length {
				break
			}
		}
	}
	return string(out), nil
}
```

Доля отбрасываемых байтов составляет 8/256 ≈ 3 %, поэтому буфер, прочитанный за одно обращение к `crypto/rand`, в подавляющем большинстве случаев содержит достаточно годных значений, и внешний цикл выполняется один раз. Равномерность распределения проверяется тестом `TestRandomUniform` (раздел 5.3). Уникальность кода обеспечивает не генератор, а ограничение `UNIQUE` в базе: `LinkService.Create` при `storage.ErrConflict` повторяет генерацию до пяти раз.

## 4.3 Разрешение ссылки: LinkService.Resolve

Метод `Resolve` реализует основной сценарий системы (рисунок 2.4). Он проверяет форму кода, читает запись из кэша, при промахе или ошибке кэша обращается к хранилищу, проверяет срок жизни и ставит событие в очередь аналитики (листинг 4.3).

Листинг 4.3 — Разрешение кода с деградацией при отказе кэша

```go
func (s *LinkService) Resolve(ctx context.Context, code string, v Visit) (string, error) {
	if !shortcode.LooksValid(code) {
		return "", ErrNotFound
	}
	now := s.now()

	entry, err := s.cache.Get(ctx, code)
	if err != nil {
		if !errors.Is(err, cache.ErrMiss) {
			// Кэш недоступен — деградируем до чтения из БД.
			s.log.Warn("cache get failed", "err", err)
		}
		entry, err = s.loadAndMaybeCache(ctx, code)
		if err != nil {
			return "", err
		}
	}
	if entry.ExpiresAt != nil && !now.Before(*entry.ExpiresAt) {
		return "", ErrGone
	}

	s.tracker.Track(analytics.Event{
		LinkID:      entry.LinkID,
		Time:        now,
		IP:          v.IP,
		UserAgent:   v.UserAgent,
		Referer:     v.Referer,
		CountryHint: v.CountryHint,
	})
	return entry.OriginalURL, nil
}
```

Любая ошибка кэша, кроме обычного промаха, лишь записывается в журнал: редирект продолжает работать за счёт PostgreSQL. Проверка `LooksValid` отсекает заведомо некорректные коды (слишком длинные, с недопустимыми символами) до обращения к кэшу и базе, что снижает нагрузку при переборе адресов. Срок жизни проверяется повторно и для записи из кэша: хотя TTL записи ограничен оставшимся временем жизни ссылки, проверка исключает редирект по просроченной ссылке при любом расхождении часов.

Политика «кэшировать после порога» реализована в `loadAndMaybeCache` (листинг 4.4). Метод читает ссылку из хранилища, отклоняет отключённые и просроченные ссылки (`ErrGone`), увеличивает счётчик обращений и при достижении порога кладёт запись в кэш с временем жизни, не превышающим оставшегося срока ссылки.

Листинг 4.4 — Решение о кэшировании записи

```go
func (s *LinkService) loadAndMaybeCache(ctx context.Context, code string) (*cache.Entry, error) {
	link, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !link.IsActive || link.Expired(s.now()) {
		return nil, ErrGone
	}
	entry := &cache.Entry{LinkID: link.ID, OriginalURL: link.OriginalURL, ExpiresAt: link.ExpiresAt}

	hits, err := s.cache.Hit(ctx, code, s.opts.HitWindow)
	if err != nil {
		s.log.Warn("cache hit counter failed", "err", err)
		return entry, nil
	}
	if hits >= s.opts.PopularThreshold {
		ttl := s.opts.CacheTTL
		if link.ExpiresAt != nil {
			if left := link.ExpiresAt.Sub(s.now()); left < ttl {
				ttl = left
			}
		}
		if ttl > 0 {
			if err := s.cache.Set(ctx, code, *entry, ttl); err != nil {
				s.log.Warn("cache set failed", "err", err)
			}
		}
	}
	return entry, nil
}
```

## 4.4 Кэш на Redis: счётчик попаданий

Метод `RedisCache.Hit` увеличивает счётчик окна и счёт кода в рейтинге. Две команды отправляются одной транзакцией-конвейером (`MULTI`/`EXEC`), что сокращает число сетевых обращений до одного; срок жизни счётчика выставляется отдельной командой только при первом обращении (листинг 4.5).

Листинг 4.5 — Счётчик обращений в Redis

```go
func (r *RedisCache) Hit(ctx context.Context, code string, window time.Duration) (int64, error) {
	var incr *redis.IntCmd
	_, err := r.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.Incr(ctx, hitsKey(code))
		p.ZIncrBy(ctx, popularKey, 1, code)
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := incr.Val()
	if n == 1 && window > 0 {
		if err := r.c.Expire(ctx, hitsKey(code), window).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}
```

Получаемое окно является фиксированным: оно открывается первым обращением и закрывается по истечении `window`, последующие обращения его не продлевают. Теоретически сбой между `INCR` и `EXPIRE` оставил бы счётчик без срока жизни; последствием был бы лишь более ранний перевод ссылки в кэш, а при нехватке памяти такой ключ вытесняется политикой `allkeys-lru`. Рейтинг для `/api/v1/top` читается командой `ZREVRANGE` (метод `TopCodes`).

## 4.5 Сборщик аналитики

Пакет `analytics` реализует асинхронный конвейер (рисунок 2.5). Метод `Track` вызывается из обработчика редиректа и не должен блокироваться, поэтому отправка в канал выполняется конструкцией `select` с веткой `default` (листинг 4.6). Читающая блокировка защищает флаг остановки: после закрытия приёма события отбрасываются, а не отправляются в канал, который уже дренируется.

Листинг 4.6 — Неблокирующая постановка события в очередь

```go
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
```

Воркер (листинг 4.7) накапливает события в пакет и сбрасывает его при достижении `BatchSize` либо по таймеру `FlushInterval`. При отмене контекста воркер сначала закрывает приём новых событий (`stop`), затем дочитывает канал до опустошения и сбрасывает остаток: именно этот дренаж гарантирует, что события, принятые до остановки, не теряются (требование НФТ-4).

Листинг 4.7 — Воркер: пакетирование и дренаж при остановке

```go
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
```

Обогащение события (`enrich`) выполняется в воркере, а не в обработчике запроса, поэтому разбор User-Agent и поиск страны не увеличивают задержку редиректа. Идентификатор посетителя вычисляется как первые 16 шестнадцатеричных символов SHA-256 от строки «соль + IP + `|` + User-Agent»; в запись `Click` IP-адрес не попадает. Соль формируется из секрета `AUTH_SECRET` с префиксом `visitor:`. Страна берётся из подсказки CDN (только если она корректна: две латинские буквы, не `XX` и не `T1`), иначе определяется по базе GeoLite2, иначе принимает значение `ZZ`.

Запись пакета (`flush`) выполняется с таймаутом 5 с: сначала `InsertBatch` (в PostgreSQL — `COPY`), затем `AddClicks` с агрегированными по ссылкам приращениями. Ошибки записи увеличивают счётчик `Failed` и логируются; повторная отправка пакета не выполняется, то есть при сбое базы данные пакета теряются — это сознательный компромисс в пользу простоты.

## 4.6 Разбор User-Agent

Для определения браузера, ОС и типа устройства применяются эвристики на основе `strings.Contains` без внешних библиотек (`internal/analytics/useragent.go`). Порядок проверок существенен: строки User-Agent Edge, Opera, Яндекс.Браузера и Samsung Internet содержат токен `Chrome/`, а Chrome — токен `Safari/`, поэтому более специфичные браузеры проверяются раньше (листинг 4.8).

Листинг 4.8 — Определение браузера и типа устройства

```go
func parseBrowser(ua string) string {
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"), strings.Contains(ua, "EdgiOS/"), strings.Contains(ua, "Edge/"):
		return "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"), strings.Contains(ua, "OPT/"):
		return "Opera"
	case strings.Contains(ua, "YaBrowser"):
		return "Yandex"
	case strings.Contains(ua, "SamsungBrowser"):
		return "Samsung Internet"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		return "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"), strings.Contains(ua, "Chromium/"):
		return "Chrome"
	case strings.Contains(ua, "Safari/"):
		return "Safari"
	}
	return "Other"
}

func parseDevice(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "bot"
	}
	l := strings.ToLower(ua)
	for _, k := range []string{"bot", "crawler", "spider", "curl", "wget", "python-requests", "go-http-client"} {
		if strings.Contains(l, k) {
			return "bot"
		}
	}
	switch {
	case strings.Contains(ua, "iPad"):
		return "tablet"
	case strings.Contains(ua, "Android") && !strings.Contains(ua, "Mobile"):
		return "tablet"
	case strings.Contains(ua, "Mobile"), strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPod"):
		return "mobile"
	}
	return "desktop"
}
```

При определении типа устройства первой выполняется проверка на роботов (пустой User-Agent считается роботом), затем планшеты: Android без токена `Mobile` относится к планшетам, как принято в User-Agent браузеров для Android. Эвристики не претендуют на полноту: нестандартные и намеренно искажённые строки попадают в категорию `Other`.

## 4.7 Аутентификация

Токен формируется как `base64url(userID.exp).base64url(HMAC-SHA256)`. При проверке подпись сравнивается за постоянное время функцией `hmac.Equal` до разбора полезной нагрузки, затем проверяется срок действия (листинг 4.9).

Листинг 4.9 — Выпуск и проверка токена

```go
func (s *Service) issue(userID int64) string {
	exp := s.now().Add(s.ttl).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d", userID, exp)))
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

func (s *Service) ParseToken(token string) (int64, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return 0, ErrInvalidToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(payload)) {
		return 0, ErrInvalidToken
	}
	// ... разбор "id.exp" ...
	if err1 != nil || err2 != nil || !s.now().Before(time.Unix(exp, 0)) {
		return 0, ErrInvalidToken
	}
	return id, nil
}
```

Пароли хэшируются bcrypt со стоимостью 12 (константа `bcryptCost` в `main.go`); допустимая длина пароля — от 8 до 72 байт (предел bcrypt). При входе несуществующего пользователя выполняется сравнение с заранее вычисленным фиктивным хэшем, а в обоих случаях отказа возвращается одна ошибка `ErrInvalidCredentials`, которая превращается в ответ `401 invalid email or password`. Тем самым и текст ответа, и время обработки не позволяют определить, зарегистрирован ли адрес.

## 4.8 HTTP-слой: маршрутизация и middleware

Маршруты регистрируются в `http.ServeMux` с использованием шаблонов Go 1.22: метод и путь задаются одной строкой, параметры пути извлекаются методом `r.PathValue` (листинг 4.10). Шаблон `GET /{code}` является самым общим; мультиплексор выбирает наиболее конкретный подходящий шаблон независимо от порядка регистрации, поэтому `/healthz`, `/metrics`, `/dashboard/` и `/api/...` не перекрываются маршрутом редиректа. Шаблон `GET /{$}` соответствует только корню.

Листинг 4.10 — Маршруты и цепочка middleware

```go
mux.HandleFunc("GET /healthz", s.handleHealth)
mux.HandleFunc("GET /metrics", s.handleMetrics)

mux.HandleFunc("POST /api/v1/auth/register", s.limited(s.handleRegister))
mux.HandleFunc("POST /api/v1/auth/login", s.limited(s.handleLogin))
mux.HandleFunc("GET /api/v1/me", s.authed(s.handleMe))
mux.HandleFunc("POST /api/v1/links", s.limited(s.authed(s.handleCreateLink)))
mux.HandleFunc("GET /api/v1/links", s.authed(s.handleListLinks))
mux.HandleFunc("GET /api/v1/links/{code}", s.authed(s.handleGetLink))
mux.HandleFunc("DELETE /api/v1/links/{code}", s.authed(s.handleDeleteLink))
mux.HandleFunc("GET /api/v1/links/{code}/stats", s.authed(s.handleStats))
mux.HandleFunc("GET /api/v1/top", s.handleTop)
// ...
mux.HandleFunc("GET /{code}", s.handleRedirect)

var h http.Handler = mux
h = limitBody(h)
h = securityHeaders(h)
h = recoverer(s.log, h)
h = accessLog(s.log, s.Metrics, s.TrustProxy, h)
h = requestID(h)
return h
```

Middleware применяются как «луковица»: последний обёрнутый является внешним, поэтому запрос последовательно проходит `requestID` → `accessLog` → `recoverer` → `securityHeaders` → `limitBody` → маршрутизатор. Порядок значим: `recoverer` расположен внутри `accessLog`, чтобы паника обработчика превращалась в ответ 500 и попадала в журнал и метрики; `requestID` — снаружи, чтобы идентификатор был доступен всем остальным. Обёртки уровня маршрута `limited` (ограничение частоты) и `authed` (проверка токена) применяются только к тем конечным точкам, которым они нужны. Ограничение частоты установлено на `POST /auth/register`, `POST /auth/login` и `POST /links`; редиректы, `/healthz` и `/metrics` не лимитируются (тест `TestRateLimit`).

Заголовки безопасности задаются middleware `securityHeaders` для всех ответов: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin` и политика `Content-Security-Policy`, разрешающая скрипты и стили только с собственного источника (`script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`). Поскольку дашборд не использует встроенных скриптов и стилей, политика не мешает его работе. Ответ редиректа дополнительно содержит `Cache-Control: private, max-age=0`.

## 4.9 Ограничение частоты запросов

Ограничитель (`internal/httpapi/ratelimit.go`) реализует алгоритм «token bucket» по ключу — IP-адресу клиента. Каждый ключ получает ведро ёмкостью `burst`, пополняемое со скоростью `rps` токенов в секунду; запрос списывает один токен. Состояние хранится в карте под мьютексом, а ведра, которые успели бы наполниться полностью, периодически удаляются, чтобы память не росла при множестве разовых клиентов (листинг 4.11).

Листинг 4.11 — Token bucket

```go
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.rps <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rps)
			b.last = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rps * float64(time.Second))
	return false, wait
}
```

При отказе обработчик возвращает `429 Too Many Requests` с заголовком `Retry-After` (в секундах) и увеличивает метрику `shortener_rate_limited_total`. Значения по умолчанию — 5 запросов в секунду и всплеск до 20 — задаются переменными `RATE_LIMIT_RPS` и `RATE_LIMIT_BURST`. IP клиента определяет функция `clientIP`: заголовок `X-Forwarded-For` (первый адрес списка) учитывается только при `TRUST_PROXY=true` и только если он является корректным IP-адресом; иначе используется `RemoteAddr`. Состояние ограничителя хранится в памяти процесса, поэтому при нескольких экземплярах приложения лимит действует на каждый из них отдельно.

## 4.10 Служебные конечные точки

Обработчик `/healthz` последовательно опрашивает зависимости (`database` и `cache`, методом `Ping`) с общим таймаутом 2 с и возвращает `200` с телом `{"status":"ok","checks":{...}}` либо `503` со статусом `unavailable`. Конечная точка `/metrics` выводит счётчики в текстовом формате Prometheus: `shortener_http_requests_total`, `shortener_http_responses_5xx_total`, `shortener_redirects_total`, `shortener_redirects_not_found_total`, `shortener_redirects_gone_total`, `shortener_rate_limited_total`, `shortener_links_created_total`, `shortener_http_in_flight` (gauge), а также счётчики сборщика `shortener_analytics_accepted_total`, `shortener_analytics_dropped_total`, `shortener_analytics_written_total` и `shortener_analytics_failed_total`. Все счётчики реализованы атомарными типами стандартной библиотеки, без внешних библиотек.

## 4.11 Миграции и пакетная запись в PostgreSQL

Схема изменяется нумерованными SQL-файлами `internal/storage/postgres/migrations/NNN_*.sql`, встроенными в бинарный файл директивой `//go:embed`. Метод `Migrate` вызывается при создании хранилища и выполняет все ещё не применённые файлы в одной транзакции под рекомендательной блокировкой, чтобы одновременный запуск нескольких экземпляров не привёл к конкурирующему применению (листинг 4.12). Таблицу `schema_migrations` создаёт сам этот код, поэтому в `001_init.sql` её нет.

Листинг 4.12 — Применение миграций под advisory lock

```go
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
// ... чтение применённых версий ...
for _, m := range list {
	if applied[m.version] {
		continue
	}
	body, err := migrationsFS.ReadFile("migrations/" + m.name)
	// ...
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("postgres: миграция %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
		return fmt.Errorf("postgres: отметка миграции %s: %w", m.name, err)
	}
}
return tx.Commit(ctx)
```

Блокировка `pg_advisory_xact_lock` снимается автоматически при завершении транзакции. Поскольку DDL в PostgreSQL транзакционен, при ошибке в любой миграции все изменения откатываются.

Пакет переходов записывается протоколом `COPY` через `pgx.CopyFrom`, а счётчики ссылок увеличиваются одним запросом с разворачиванием массивов (листинг 4.13).

Листинг 4.13 — Пакетная вставка переходов и обновление счётчиков

```go
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

// AddClicks (фрагмент)
_, err := s.pool.Exec(ctx,
	`UPDATE links SET click_count = click_count + d.delta
	 FROM unnest($1::bigint[], $2::bigint[]) AS d(id, delta)
	 WHERE links.id = d.id`, ids, vals)
```

## 4.12 Запросы статистики

Агрегаты по ссылке вычисляются средствами СУБД за полуинтервал `[from, to)`; каждый запрос использует составной индекс `idx_clicks_link_time` (листинг 4.14). Запросы разрезов заданы константами, значения передаются параметрами (`$1`…`$4`), конкатенация строк SQL не применяется, что исключает SQL-инъекции.

Листинг 4.14 — Запросы статистики

```go
const qByCountry = `SELECT trim(country) AS k, count(*) AS n FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3 GROUP BY k ORDER BY n DESC, k LIMIT $4`
// аналогично qByReferrer, qByBrowser, qByOS, qByDevice

`SELECT count(*), count(DISTINCT visitor_id) FROM clicks
 WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3`

`SELECT date_trunc('day', clicked_at AT TIME ZONE 'UTC') AS d, count(*)
 FROM clicks WHERE link_id = $1 AND clicked_at >= $2 AND clicked_at < $3
 GROUP BY d ORDER BY d`
```

Функция `trim(country)` нужна потому, что значение типа `CHAR(2)` может дополняться пробелами. Дни группируются по UTC, ограничение `LIMIT $4` задаёт размер рейтингов (`StatsTopN`, по умолчанию 10). Сервисный слой дополнительно проверяет, что период не пуст и не превышает 366 суток (`ErrBadPeriod`).

## 4.13 Запуск и корректная остановка

Точка входа (`cmd/shortener/main.go`) загружает конфигурацию, выбирает реализации хранилища и кэша (PostgreSQL и Redis при заданных `DATABASE_URL` и `REDIS_URL`, иначе реализации в памяти), создаёт сборщик аналитики и запускает его в отдельной горутине, собирает сервисы и HTTP-сервер с таймаутами (`ReadHeaderTimeout` 5 с, `ReadTimeout` 15 с, `WriteTimeout` 30 с, `IdleTimeout` 90 с). Остановка по сигналу выполняется в порядке, показанном в листинге 4.15: сначала перестаёт принимать запросы HTTP-сервер, затем останавливается и дренируется сборщик, и лишь после этого (через `defer`) закрываются кэш и хранилище, которые сборщик использует при записи.

Листинг 4.15 — Корректная остановка

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
// ... сборка зависимостей ...
collectorCtx, cancelCollector := context.WithCancel(context.Background())
var collectorDone sync.WaitGroup
collectorDone.Add(1)
go func() {
	defer collectorDone.Done()
	collector.Run(collectorCtx)
}()
// ...
select {
case <-ctx.Done():
	log.Info("получен сигнал остановки")
case err := <-serveErr:
	if err != nil {
		runErr = fmt.Errorf("http-сервер: %w", err)
	}
}

// Корректная остановка: сервер -> сборщик (дренаж очереди) -> store/cache (defer).
shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
defer cancel()
if err := srv.Shutdown(shutdownCtx); err != nil {
	log.Error("остановка http-сервера", "err", err)
}
cancelCollector()
collectorDone.Wait()
```

Сборщик получает собственный контекст, не связанный с контекстом сигнала: иначе он начал бы останавливаться одновременно с сервером и мог бы потерять события запросов, ещё обрабатываемых при `Shutdown`. Тайм-аут остановки сервера — 15 с.

## 4.14 Дашборд

Интерфейс владельца реализован на чистых HTML, CSS и JavaScript без сборщиков и фреймворков (файлы `web/static/index.html`, `style.css`, `app.js`) и встраивается в бинарный файл директивой `//go:embed static`. Сервис раздаёт его по адресу `/dashboard/` с заголовком `Cache-Control: no-cache`. Токен доступа хранится в `localStorage`; при ответе `401` на защищённый запрос выполняется выход.

Все данные, введённые пользователями (названия, адреса), вставляются в DOM через `textContent` и `setAttribute`; `innerHTML` не используется, поэтому внедрение разметки невозможно. Вспомогательная функция `el` создаёт элементы, присваивая текст свойством `textContent`. Дополнительно действует строгая политика CSP, запрещающая встроенные скрипты. Хранение токена в `localStorage` доступно скриптам страницы, поэтому защита от XSS (отсутствие `innerHTML` и CSP) существенна и для сохранности токена.

График переходов по дням построен вручную в формате SVG, без библиотек (листинг 4.16). Пропущенные дни заполняются нулями, верх шкалы выбирается функцией «красивого» шага, высота столбца пропорциональна числу переходов, подпись дня и значение выводятся во всплывающем элементе `<title>`.

Листинг 4.16 — Построение SVG-графика

```js
function drawChart(host, byDay, from, to) {
  host.textContent = "";
  var counts = {};
  byDay.forEach(function (d) { counts[String(d.day).slice(0, 10)] = d.count; });
  // ... список дней от from до to, пропуски — нули ...
  var W = 720, H = 220, L = 38, R = 8, T = 10, B = 28;
  var root = svg("svg", { viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "График переходов по дням" });
  // ...
  var step = niceStep(max / 4);
  var top = Math.ceil(max / step) * step;
  var plotW = W - L - R, plotH = H - T - B;
  // ... линии сетки и подписи шкалы ...
  var slot = plotW / days.length;
  var bw = Math.max(1, Math.min(28, slot * 0.7));
  days.forEach(function (d, i) {
    var h = (d.count / top) * plotH;
    var x = L + i * slot + (slot - bw) / 2;
    var bar = svg("rect", {
      x: x, y: T + plotH - h, width: bw, height: Math.max(h, d.count > 0 ? 1 : 0), rx: 2, "class": "bar"
    });
    bar.appendChild(svg("title", { text: d.key + ": " + d.count }));
    root.appendChild(bar);
    // ... подпись по оси X через каждые labelEvery столбцов ...
  });
  host.appendChild(root);
}
```

Атрибут `viewBox` делает график масштабируемым: на узких экранах он сжимается вместе с контейнером без перерисовки. Разрезы по странам, источникам, браузерам, ОС и устройствам выводятся горизонтальными полосами функцией `drawBars` с подписью числа и доли.

## 4.15 Контейнеризация

Образ собирается в два этапа (`Dockerfile`). Первый этап (`golang:1.27-alpine`) копирует `go.mod` и `go.sum` и загружает зависимости отдельным слоем, который переиспользуется, пока манифесты не изменились, затем собирает статический бинарный файл. Второй этап использует образ `gcr.io/distroless/static-debian12:nonroot`, в котором нет оболочки и пакетного менеджера; процесс запускается от непривилегированного пользователя (листинг 4.17).

Листинг 4.17 — Dockerfile

```dockerfile
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN mkdir -p /out \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shortener ./cmd/shortener

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shortener /shortener
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/shortener"]
```

Файл `docker-compose.yml` описывает сервисы `app`, `postgres` (образ `postgres:17-alpine`, том `pgdata`) и `redis` (образ `redis:7-alpine`, параметры `--maxmemory 128mb --maxmemory-policy allkeys-lru`). Для баз заданы проверки работоспособности `pg_isready` и `redis-cli ping` (интервал 5 с, до 10 попыток), а приложение зависит от них с условием `service_healthy` и поэтому не стартует раньше готовности (листинг 4.18). Переменная `AUTH_SECRET` объявлена обязательной: при её отсутствии Compose прекращает запуск с сообщением об ошибке.

Листинг 4.18 — Фрагмент docker-compose.yml

```yaml
services:
  app:
    build: .
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://shortener:shortener@postgres:5432/shortener?sslmode=disable
      REDIS_URL: redis://redis:6379/0
      AUTH_SECRET: ${AUTH_SECRET:?Задайте AUTH_SECRET в файле .env (см. .env.example)}
      BASE_URL: http://localhost:8080
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    restart: unless-stopped
```

## 4.16 Непрерывная интеграция

Конвейер GitHub Actions (`.github/workflows/ci.yml`) запускается при каждом `push` и `pull request`, права токена ограничены чтением (`contents: read`). Состав заданий приведён в таблице 4.1.

Таблица 4.1 — Задания конвейера непрерывной интеграции

| Задание | Содержание | Результат |
|---|---|---|
| `test` | `go vet ./...`; `go test -race -coverprofile=coverage.out ./...` | отчёт о покрытии как артефакт |
| `integration` | сервис `postgres:17-alpine` с проверкой готовности; `go test -tags integration ./...` с `TEST_DATABASE_URL` | проверка хранилища на реальной СУБД |
| `lint` | `golangci-lint` (действие `golangci-lint-action@v8`) по `.golangci.yml` | замечания линтеров |
| `security` | `gosec ./...` и `govulncheck ./...` | SAST и поиск известных уязвимостей |
| `docker` | сборка образа без публикации; запускается после успешного `test` | проверка воспроизводимости сборки |

Версия Go во всех заданиях берётся из `go.mod` (`go-version-file`), поэтому закрепление версии инструментальной цепочки в модуле автоматически применяется и в конвейере. Дополнительно Dependabot еженедельно предлагает обновления модулей Go, действий GitHub Actions и образов Docker.

## 4.17 Пользовательский интерфейс

Экран входа и регистрации (рисунок 4.1) содержит две вкладки и форму из полей email и пароля; сообщения об ошибках выводятся под формой.

![](../screenshots/01-login.png)

Рисунок 4.1 — Экран входа в личный кабинет

После входа открывается основной экран (рисунок 4.2): форма создания ссылки (адрес, необязательные алиас, заголовок и срок действия), таблица «Мои ссылки» с кнопками копирования, просмотра статистики и удаления и, при выборе ссылки, карточка статистики с суммарным числом переходов и уникальных посетителей, графиком по дням и рейтингами стран, источников, браузеров, ОС и устройств. Период задаётся датами или быстрыми кнопками на 7, 30 и 90 дней.

![](../screenshots/02-dashboard-light.png)

Рисунок 4.2 — Дашборд со статистикой по ссылке (светлая тема)

Тема оформления определяется настройкой системы пользователя (`prefers-color-scheme`): при тёмной теме применяется соответствующая палитра (рисунок 4.3).

![](../screenshots/03-dashboard-dark.png)

Рисунок 4.3 — Дашборд в тёмной теме

Вёрстка адаптивна: на узких экранах блоки располагаются в один столбец, график масштабируется, а таблица ссылок получает горизонтальную прокрутку (рисунок 4.4).

![](../screenshots/04-mobile.png)

Рисунок 4.4 — Мобильная версия дашборда

## 4.18 Выводы по главе 4

1. Реализация соответствует проекту: слои связаны интерфейсами, сборка конкретных реализаций выполнена в одной точке — `cmd/shortener/main.go`.
2. Критический путь редиректа не содержит записи в базу: запись статистики вынесена в пул воркеров с пакетной вставкой через `COPY` и дренажом при остановке.
3. Безопасность обеспечена на нескольких уровнях: ограничение частоты, подпись токенов с сравнением за постоянное время, bcrypt, параметризованные запросы, вывод через `textContent`, заголовки безопасности и строгая политика CSP.
4. Развёртывание сведено к одной команде благодаря многоэтапному образу `distroless`, Compose с проверками работоспособности и конвейеру CI из пяти заданий.
