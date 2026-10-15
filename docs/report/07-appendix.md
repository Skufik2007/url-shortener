# Приложения

## Приложение А. Инструкция по запуску

### А.1 Демонстрационный режим (без внешних сервисов)

Для запуска требуется Go версии 1.27 или новее. Если `DATABASE_URL` и `REDIS_URL` не заданы, сервис использует хранилище и кэш в памяти; без дополнительных настроек данные не сохраняются между перезапусками. Если не задан и `AUTH_SECRET`, при каждом запуске генерируется случайный секрет (токены перестают действовать после перезапуска).

```
git clone https://github.com/Skufik2007/url-shortener.git
cd url-shortener
go run ./cmd/shortener
```

После запуска дашборд доступен по адресу `http://localhost:8080/` (перенаправление на `/dashboard/`). Для запуска с другим адресом задаются переменные окружения `HTTP_ADDR` и `BASE_URL`, например `HTTP_ADDR=127.0.0.1:8091 BASE_URL=http://localhost:8091`. Сборка бинарного файла: `make build` (результат — `bin/shortener`).

Чтобы данные демонстрационного режима переживали перезапуск, задаётся переменная `DATA_FILE` с путём к JSON-файлу (при пустом `DATABASE_URL`):

```
DATA_FILE=data.json go run ./cmd/shortener
```

При запуске данные загружаются из файла, после изменений (с задержкой около 0,5 с) и при остановке сохраняются обратно. Если `AUTH_SECRET` не задан, секрет подписи токенов хранится в файле `data.json.secret` (создаётся автоматически с правами 0600), поэтому вход тоже сохраняется. Для сброса данных достаточно остановить сервис и удалить оба файла; для резервной копии — скопировать их. `DATA_FILE` вместе с `DATABASE_URL` — ошибка конфигурации; режим предназначен только для демонстрации, для продакшена применяется PostgreSQL (раздел А.2). В Windows-поставке `Запустить.bat` и `Запустить для телефона.bat` задают `DATA_FILE=data.json` и хранят файлы рядом с `shortener.exe`.

### А.2 Docker Compose (PostgreSQL и Redis)

Требуются Docker и Docker Compose. Секрет подписи токенов обязателен.

```
cp .env.example .env
# в файле .env задать AUTH_SECRET: строка не короче 32 байт, например, openssl rand -hex 32
docker compose up -d --build
```

Будут запущены три контейнера: `app` (порт 8080), `postgres` (PostgreSQL 17, том `pgdata`) и `redis` (Redis 7, `maxmemory 128mb`, `allkeys-lru`). Приложение стартует после успешных проверок работоспособности баз и самостоятельно применяет миграции. Проверка состояния: `curl http://localhost:8080/healthz`. Остановка: `docker compose down` (флаг `-v` дополнительно удаляет данные PostgreSQL).

### А.3 Основные параметры конфигурации

Параметры задаются переменными окружения (таблица 2.6): `HTTP_ADDR`, `BASE_URL`, `DATABASE_URL`, `DATA_FILE`, `REDIS_URL`, `AUTH_SECRET`, `TOKEN_TTL`, `GEOIP_DB`, `TRUST_PROXY`, `CODE_LENGTH`, `ANALYTICS_WORKERS`, `ANALYTICS_BUFFER`, `ANALYTICS_BATCH`, `ANALYTICS_FLUSH`, `RATE_LIMIT_RPS`, `RATE_LIMIT_BURST`, `LOG_LEVEL`. Если сервис работает за обратным прокси, следует установить `TRUST_PROXY=true`.

### А.4 Тесты и проверки

```
go test ./...                                   # модульные тесты
go test -race ./...                             # с детектором гонок
go test -tags integration ./...                 # интеграционные (нужен TEST_DATABASE_URL)
go test -run='^$' -bench=. -benchmem ./internal/service/ ./internal/httpapi/
go run ./cmd/loadgen -url http://127.0.0.1:8080/<код> -c 64 -d 10s
```

## Приложение Б. Спецификация REST API

Базовый путь — `/api/v1`. Тела запросов и ответов — JSON в кодировке UTF-8. Защищённые конечные точки требуют заголовок `Authorization: Bearer <token>`. Ошибки возвращаются в формате `{"error": "описание"}`. Время — в формате RFC 3339. Примеры значений условные.

### Б.1 Регистрация

`POST /api/v1/auth/register`

```
{"email": "demo@example.com", "password": "correct-horse"}
```

Ответ `201 Created`:

```
{"token": "MS4xNzkyMDAwMDAw.q3h7...", "user": {"id": 1, "email": "demo@example.com", "created_at": "2026-10-15T10:00:00Z"}}
```

Ошибки: `400` (`invalid email`, `password must be 8 to 72 bytes long`, `invalid JSON body`), `409` (`email already registered`), `413`, `429`.

### Б.2 Вход

`POST /api/v1/auth/login` — тело как при регистрации. Ответ `200 OK` в том же формате. Ошибка `401`: `{"error": "invalid email or password"}` (одинаково для неверного email и неверного пароля), `429`.

### Б.3 Текущий пользователь

`GET /api/v1/me` → `200 OK`: `{"id": 1}`. Без действительного токена — `401` с заголовком `WWW-Authenticate: Bearer realm="shortener"`.

### Б.4 Создание ссылки

`POST /api/v1/links`

```
{"url": "https://go.dev/doc/", "alias": "godoc", "title": "Документация Go", "expires_at": "2027-01-01T00:00:00Z"}
```

Обязательно только поле `url`; `alias` (3–32 символа `A-Za-z0-9_-`, не начинается с `-` и `_`, не зарезервирован), `title` (до 200 символов) и `expires_at` (в будущем) необязательны. Ответ `201 Created`, заголовок `Location: <BASE_URL>/api/v1/links/godoc`:

```
{
  "id": 4,
  "code": "godoc",
  "original_url": "https://go.dev/doc/",
  "owner_id": 1,
  "title": "Документация Go",
  "created_at": "2026-10-15T10:05:00Z",
  "expires_at": "2027-01-01T00:00:00Z",
  "is_active": true,
  "click_count": 0,
  "short_url": "http://localhost:8080/godoc"
}
```

Ошибки: `400` (недопустимый адрес или схема, ссылка на сам сервис, некорректный алиас, срок в прошлом), `401`, `409` (`alias already taken`), `413`, `429`. Поля `title` и `expires_at` в ответе опускаются, если не заданы.

### Б.5 Список ссылок

`GET /api/v1/links?limit=50&offset=0` — активные ссылки владельца, новые первыми; `limit` от 1 до 100 (по умолчанию 50), `offset` не меньше 0. Ответ `200 OK`:

```
{"links": [ { ...объект ссылки, как в Б.4... } ]}
```

Ошибки: `400` (`invalid limit`, `invalid offset`), `401`.

### Б.6 Получение и удаление ссылки

`GET /api/v1/links/{code}` → `200 OK` с объектом ссылки (как в Б.4); `404` — ссылка не найдена, отключена или принадлежит другому пользователю.

`DELETE /api/v1/links/{code}` → `204 No Content` (ссылка деактивируется, запись удаляется из кэша); повторное удаление и чужая ссылка — `404`.

### Б.7 Статистика

`GET /api/v1/links/{code}/stats?from=2026-09-16&to=2026-10-15` — даты в формате `YYYY-MM-DD`, граница `to` включается; по умолчанию — последние 30 суток; максимальный период — 366 суток. Ответ `200 OK`:

```
{
  "code": "godoc",
  "from": "2026-09-16T00:00:00Z",
  "to": "2026-10-16T00:00:00Z",
  "total_clicks": 163,
  "unique_visitors": 153,
  "by_day": [{"day": "2026-10-14T00:00:00Z", "count": 163}],
  "by_country": [{"key": "RU", "count": 61}, {"key": "UZ", "count": 26}],
  "by_referrer": [{"key": "direct", "count": 44}, {"key": "vk.com", "count": 30}],
  "by_browser": [{"key": "Safari", "count": 67}],
  "by_os": [{"key": "Windows", "count": 55}],
  "by_device": [{"key": "desktop", "count": 92}]
}
```

Списки `by_*` (кроме `by_day`) отсортированы по убыванию числа переходов и ограничены десятью элементами. Ошибки: `400` (`invalid 'from' date...`, `invalid 'to' date...`, `invalid period`), `401`, `404`.

### Б.8 Популярные коды

`GET /api/v1/top?n=10` (без аутентификации; `n` от 1 до 100, по умолчанию 10) → `200 OK`: `{"codes": ["godoc", "abc1234"]}`; при неверном `n` — `400`.

### Б.9 Редирект

`GET /{code}` → `302 Found` с заголовками `Location: <исходный адрес>` и `Cache-Control: private, max-age=0`. Если код не найден или недопустим — `404` (`{"error": "link not found"}`); если ссылка отключена или истекла — `410` (`{"error": "link expired or disabled"}`).

### Б.10 QR-код ссылки

`GET /qr/{code}.png?size=256` (без аутентификации; `size` — сторона изображения в пикселях от 64 до 1024, по умолчанию 256). В QR-коде закодирована короткая ссылка `BASE_URL/{code}`; выдача кода не считается переходом.

```
curl -o qr.png 'http://localhost:8080/qr/my-link.png?size=512'
```

| Код | Условие | Тело |
|---|---|---|
| `200 OK` | ссылка существует, активна и не истекла | PNG-изображение (`Content-Type: image/png`, `Cache-Control: public, max-age=3600`) |
| `400 Bad Request` | `size` не число или вне диапазона 64–1024 | `{"error": "size must be between 64 and 1024"}` |
| `404 Not Found` | код не найден или недопустим, либо в адресе нет расширения `.png` | `{"error": "link not found"}` |
| `410 Gone` | ссылка отключена или истекла | `{"error": "link expired or disabled"}` |

Сторона изображения кратна размеру модуля и не превышает запрошенную; вокруг кода оставлена белая рамка шириной четыре модуля. Число выданных изображений учитывается метрикой `shortener_qr_generated_total`.

### Б.11 Служебные конечные точки

`GET /healthz` → `200 OK`: `{"status": "ok", "checks": {"database": "ok", "cache": "ok"}}`; при отказе зависимости — `503`: `{"status": "unavailable", "checks": {"database": "error", "cache": "ok"}}`.

`GET /metrics` → `200 OK`, `Content-Type: text/plain; version=0.0.4`:

```
# HELP shortener_redirects_total Успешные редиректы.
# TYPE shortener_redirects_total counter
shortener_redirects_total 198378
```

Полный перечень метрик приведён в разделе 4.11. Ответ `429` на лимитируемых конечных точках содержит заголовок `Retry-After` (секунды).

## Приложение В. SQL-миграция `001_init.sql`

Таблица `schema_migrations` создаётся кодом применения миграций и в файле отсутствует.

```sql
-- Начальная схема сервиса сокращения ссылок.

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE links (
    id           BIGSERIAL PRIMARY KEY,
    code         VARCHAR(32) NOT NULL UNIQUE,
    original_url TEXT NOT NULL,
    owner_id     BIGINT REFERENCES users(id) ON DELETE CASCADE,
    title        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    is_active    BOOLEAN NOT NULL DEFAULT true,
    click_count  BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX idx_links_owner_created ON links (owner_id, created_at DESC);

CREATE TABLE clicks (
    id         BIGSERIAL PRIMARY KEY,
    link_id    BIGINT NOT NULL REFERENCES links(id) ON DELETE CASCADE,
    clicked_at TIMESTAMPTZ NOT NULL,
    referrer   TEXT NOT NULL DEFAULT '',
    country    CHAR(2) NOT NULL DEFAULT 'ZZ',
    browser    TEXT NOT NULL DEFAULT '',
    os         TEXT NOT NULL DEFAULT '',
    device     TEXT NOT NULL DEFAULT '',
    visitor_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_clicks_link_time ON clicks (link_id, clicked_at);
```

## Приложение Г. История коммитов

Вывод команды `git log --oneline --graph develop` (ветка `develop`, на момент подготовки записки). Разработка велась в тематических ветках `feature/*`, влитых в `develop` слиянием.

```
*   edf68cd Merge branch 'feature/docs' into develop
|\
| * e98fde8 docs: README, лицензия MIT и скриншоты дашборда
|/
*   a55b9e3 Merge branch 'feature/perf' into develop
|\
| * a0e3de2 feat(loadgen): генератор нагрузки с перцентилями задержки
| * 74fb7b5 test: бенчмарки разрешения ссылок и HTTP-редиректа
|/
* 000e950 chore: единые окончания строк LF через .gitattributes
*   6cc2ef0 Merge branch 'feature/infra' into develop
|\
| * 9d15dbc test: сценарий нагрузочного тестирования k6
| * 261fc36 ci: GitHub Actions (тесты, интеграция, линтер, gosec, govulncheck, сборка образа) и dependabot
| * 9bafeb8 build: Docker, Docker Compose, Makefile и конфигурация golangci-lint
|/
*   19c15e2 Merge branch 'feature/http-api' into develop
|\
| * 6ff1478 feat(cmd): точка входа с graceful shutdown
| * 88b60d0 feat(web): дашборд владельца ссылок со статистикой
| * 0e44e8f feat(api): REST API, редиректы, middleware, rate limiting и метрики Prometheus
| * c008e1b feat(config): загрузка конфигурации из переменных окружения
|/
*   f78613a Merge branch 'feature/service' into develop
|\
| * 800fe90 feat(service): бизнес-логика ссылок с кэшированием популярных и деградацией при отказе Redis
|/
*   a49150f Merge branch 'feature/auth' into develop
|\
| * b060d9d feat(auth): регистрация и вход с bcrypt, HMAC-подписанные токены
|/
*   6a67ccd Merge branch 'feature/analytics' into develop
|\
| * 6d89eda feat(analytics): асинхронный сборщик переходов с пулом воркеров и пакетной записью
| * d9f385d feat(analytics): разбор User-Agent, домена реферера и GeoIP
|/
*   654be95 Merge branch 'feature/shortcode-cache' into develop
|\
| * 1fbca16 feat(cache): кэш популярных ссылок на Redis и в памяти, счётчики обращений
| * b7608d2 feat(shortcode): генератор base62 на crypto/rand и валидация алиасов
|/
*   8e17674 Merge branch 'feature/storage' into develop
|\
| * 635a62f feat(storage): хранилище PostgreSQL на pgx с встроенными миграциями и COPY-вставкой
| * a35b80c feat(storage): потокобезопасное хранилище в памяти для тестов и демо-режима
|/
*   8ed907e Merge branch 'feature/domain' into develop
|\
| * 0385c07 feat(domain): доменные модели и интерфейсы хранилища, кэша и аналитики
|/
* e0f0532 chore: инициализация модуля Go и проектный документ архитектуры
```
