# Архитектура url-shortener (контракт для разработки)

Курсовой проект, вариант 29: «Сервис сокращения ссылок с аналитикой» (аналог bit.ly).
Стек: Go 1.27 (только stdlib `net/http` с маршрутизацией по шаблонам), PostgreSQL (pgx/v5),
Redis (go-redis/v9). Модуль: `github.com/Skufik2007/url-shortener`.

Комментарии в коде — на русском, имена — на английском. Логирование — `log/slog`.
Каждый пакет покрывается модульными тестами (`go test ./...`), без внешних сервисов:
Postgres-тесты помечаются build-тегом `integration`, Redis тестируется через miniredis.

## Пакеты

| Пакет | Назначение |
|---|---|
| `internal/model` | доменные сущности (User, Link, Click, LinkStats) — **готово** |
| `internal/storage` | интерфейсы `Store`, ошибки `ErrNotFound`/`ErrConflict` — **готово** |
| `internal/storage/memory` | потокобезопасная реализация `storage.Store` в памяти (sync.RWMutex) |
| `internal/storage/postgres` | реализация на pgxpool + миграции (embed `migrations/*.sql`) |
| `internal/cache` | интерфейс `LinkCache` — **готово**; `RedisCache`, `MemoryCache` |
| `internal/shortcode` | генерация base62-кодов, валидация алиасов (контракт в shortcode.go) |
| `internal/analytics` | `Collector` (канал + пул воркеров + пакетная запись), парсинг UA, реферера, гео |
| `internal/auth` | регистрация/вход, bcrypt, подписанные HMAC-токены |
| `internal/service` | бизнес-логика `LinkService` — **готово** |
| `internal/httpapi` | HTTP-обработчики, middleware, раздача дашборда |
| `internal/config` | конфигурация из переменных окружения |
| `web/` | дашборд (HTML + vanilla JS, встраивается через `embed`) |
| `cmd/shortener` | точка входа, сборка зависимостей, graceful shutdown |

## Ключевые потоки

**Редирект `GET /{code}`**: `service.Resolve` → кэш Redis (`link:{code}`) → при промахе
Postgres → счётчик `hits:{code}` (INCR + EXPIRE окна); если ≥ порога — ссылка кладётся в кэш
(TTL ≤ срока жизни ссылки). Событие перехода уходит в `analytics.Collector.Track`
(неблокирующая запись в буферизованный канал; при переполнении событие отбрасывается и
увеличивается счётчик `dropped`). Ответ — `302 Found` + `Cache-Control: private, max-age=0`
(чтобы браузер не кэшировал редирект и каждый переход учитывался).

**Сбор аналитики**: N воркеров читают канал, обогащают событие: реферер → домен
(`direct`, если пусто или свой хост), User-Agent → браузер/ОС/тип устройства (эвристики,
боты → `bot`), страна → `CountryHint` из заголовка, иначе MaxMind GeoLite2 (если задан
`GEOIP_DB`), иначе `ZZ`; `VisitorID` = первые 16 hex-символов SHA-256(соль + IP + UA)
(IP не хранится — персональные данные). Пакет пишется при наборе `BatchSize` событий или
по таймеру `FlushInterval`: `InsertBatch` + `AddClicks` (агрегированные дельты).
При остановке (`ctx` отменён) канал дочитывается и сбрасывается.

**Аутентификация**: `POST /api/v1/auth/register|login` → токен
`base64url(userID.expUnix).base64url(HMAC-SHA256(secret, payload))`; передаётся в
`Authorization: Bearer <token>`. Пароли — bcrypt (cost 12 в проде, `bcrypt.MinCost` в тестах).

## HTTP API (`/api/v1`, JSON)

| Метод | Путь | Описание |
|---|---|---|
| POST | `/auth/register` | `{email, password}` → `201 {token, user}`; пароль ≥ 8 символов |
| POST | `/auth/login` | `{email, password}` → `200 {token, user}` / `401` |
| GET | `/me` | текущий пользователь |
| POST | `/links` | `service.CreateInput` → `201 {link..., short_url}` |
| GET | `/links?limit&offset` | список ссылок владельца |
| GET | `/links/{code}` | одна ссылка |
| DELETE | `/links/{code}` | `204` |
| GET | `/links/{code}/stats?from=YYYY-MM-DD&to=YYYY-MM-DD` | `model.LinkStats`; по умолчанию последние 30 дней, `to` включительно |
| GET | `/top?n=10` | популярные коды (публично) |

Служебные: `GET /healthz` (проверка БД и кэша), `GET /metrics` (текстовые счётчики в
формате Prometheus: запросы, редиректы, промахи кэша, отброшенные события),
`GET /dashboard/` (статический дашборд), `GET /` → 302 на `/dashboard/`.

Ошибки: `{"error": "текст"}`; соответствие: `ErrInvalidURL`/`ErrBadAlias`/`ErrBadPeriod` → 400,
`ErrNotFound` → 404, `ErrGone` → 410, `ErrAliasTaken` → 409, нет токена → 401.
Middleware: request-id, recover, access-log (slog), ограничение размера тела (64 КБ),
rate limiting (token bucket по IP) на `POST /links` и `/auth/*`, заголовки безопасности.

## Конфигурация (env)

`HTTP_ADDR` (`:8080`), `BASE_URL` (`http://localhost:8080`), `DATABASE_URL` (пусто → память),
`REDIS_URL` (пусто → память), `AUTH_SECRET` (обязателен, если задан DATABASE_URL),
`TOKEN_TTL` (`72h`), `GEOIP_DB`, `TRUST_PROXY` (`false`), `CODE_LENGTH` (`7`),
`ANALYTICS_WORKERS` (`4`), `ANALYTICS_BUFFER` (`10000`), `ANALYTICS_BATCH` (`500`),
`ANALYTICS_FLUSH` (`1s`), `RATE_LIMIT_RPS` (`5`), `RATE_LIMIT_BURST` (`20`), `LOG_LEVEL` (`info`).

## Схема БД

```sql
users(id BIGSERIAL PK, email TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, created_at TIMESTAMPTZ DEFAULT now())
links(id BIGSERIAL PK, code VARCHAR(32) UNIQUE NOT NULL, original_url TEXT NOT NULL,
      owner_id BIGINT REFERENCES users(id) ON DELETE CASCADE, title TEXT NOT NULL DEFAULT '',
      created_at TIMESTAMPTZ DEFAULT now(), expires_at TIMESTAMPTZ, is_active BOOLEAN DEFAULT true,
      click_count BIGINT DEFAULT 0)
      INDEX (owner_id, created_at DESC)
clicks(id BIGSERIAL PK, link_id BIGINT REFERENCES links(id) ON DELETE CASCADE,
       clicked_at TIMESTAMPTZ NOT NULL, referrer TEXT, country CHAR(2), browser TEXT, os TEXT,
       device TEXT, visitor_id TEXT)
       INDEX (link_id, clicked_at)
schema_migrations(version INT PK, applied_at TIMESTAMPTZ)
```
