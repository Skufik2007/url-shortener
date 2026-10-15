# url-shortener — сервис сокращения ссылок с аналитикой

Курсовая работа по дисциплине «Методы программирования». Сервис на Go (только стандартная
библиотека плюс драйверы PostgreSQL/Redis), который создаёт короткие ссылки, выполняет
редирект и собирает статистику переходов.

## Возможности

- Сокращение URL: случайный код (по умолчанию 7 символов) или собственный алиас, заголовок,
  срок действия.
- Быстрый редирект `GET /{code}` → `302 Found`; популярные ссылки кэшируются (Redis или память).
- Аналитика переходов: количество, география (страна), реферер, браузер, ОС, тип устройства,
  уникальные посетители (IP не хранится — только хэш с солью). Запись асинхронная, пакетная.
- Регистрация и вход по e-mail и паролю (bcrypt), токены HMAC-SHA256 в `Authorization: Bearer`.
- QR-коды ссылок: `GET /qr/{code}.png?size=256` отдаёт PNG (уровень коррекции M, размер 64–1024 px);
  в дашборде — кнопка «QR» с окном просмотра и скачивания; показ QR не считается переходом.
- Веб-дашборд (светлая и тёмная темы, адаптивная вёрстка), встроен в бинарник.
- Ограничение частоты запросов по IP, заголовки безопасности, лимит размера тела,
  метрики `/metrics` и проверка состояния `/healthz`, корректная остановка.
- Демо-режим без внешних зависимостей: хранилище и кэш в памяти; при заданном `DATA_FILE` данные сохраняются
  в JSON-файл и переживают перезапуск.

## Архитектура

```mermaid
flowchart LR
    C[Клиент / браузер] --> H[httpapi<br/>middleware + маршруты]
    H --> A[auth]
    H --> S[service<br/>LinkService]
    S --> K[(cache<br/>Redis / память)]
    S --> D[(storage<br/>PostgreSQL / память)]
    S -- Track --> Q[analytics.Collector<br/>канал + воркеры]
    Q -- пакетная запись --> D
```

Подробное описание пакетов, потоков данных, HTTP API и схемы БД — в
[docs/DESIGN.md](docs/DESIGN.md).

Руководство пользователя (запуск, работа с дашбордом, решение проблем) — в
[docs/USER_GUIDE.md](docs/USER_GUIDE.md).

## Быстрый старт

### Демо-режим (без БД)

Нужен Go 1.27 или новее. Без дополнительных настроек данные хранятся только в памяти и пропадают
при остановке, а секрет токенов генерируется при каждом запуске.

```bash
go run ./cmd/shortener
```

### Демо-режим с сохранением данных

Если задать `DATA_FILE`, данные записываются в JSON-файл и загружаются при следующем запуске, а секрет
подписи токенов хранится рядом (`<DATA_FILE>.secret`, права 0600), поэтому вход тоже сохраняется:

```bash
DATA_FILE=data.json go run ./cmd/shortener
```

Запись атомарная и происходит с задержкой около 0,5 с после изменений и при корректной остановке.
Повреждённый файл не перезаписывается: сервис не запустится и сообщит об ошибке.

### Режимы хранения

| Режим | Условие | Назначение |
|---|---|---|
| PostgreSQL | задан `DATABASE_URL` | продакшен, данные в БД |
| Память | `DATABASE_URL` и `DATA_FILE` пусты | тесты и быстрый запуск, данные теряются при остановке |
| Память со снимком в файле | задан `DATA_FILE`, `DATABASE_URL` пуст | демонстрация, данные переживают перезапуск |

`DATA_FILE` вместе с `DATABASE_URL` — ошибка конфигурации.

Дашборд: <http://localhost:8080/dashboard/>.

### QR-коды и телефон

Кнопка «QR» в строке ссылки открывает окно с кодом; тот же PNG доступен напрямую:

```bash
curl -o qr.png 'http://localhost:8080/qr/my-link.png?size=512'
```

В QR кодируется `BASE_URL` + `/` + код. Чтобы ссылку можно было открыть с телефона, `BASE_URL`
должен указывать на адрес, доступный по сети (на телефоне `localhost` — сам телефон), например
`BASE_URL=http://192.168.1.15:8080` и `HTTP_ADDR=0.0.0.0:8080`. В Windows-поставке для этого есть
`Запустить для телефона.bat`, который сам определяет IP ноутбука в локальной сети.

### Docker Compose (PostgreSQL + Redis)

```bash
cp .env.example .env
# задайте в .env длинный случайный AUTH_SECRET, например: openssl rand -hex 32
docker compose up -d --build
```

Без `AUTH_SECRET` Compose не запустится. Сервис будет доступен на порту 8080. Остановка:
`docker compose down` (том `pgdata` сохраняется).

## Переменные окружения

| Переменная | По умолчанию | Описание |
|---|---|---|
| `HTTP_ADDR` | `:8080` | адрес HTTP-сервера |
| `BASE_URL` | `http://localhost:8080` | публичный URL, из которого строятся короткие ссылки |
| `DATABASE_URL` | пусто | строка подключения PostgreSQL; пусто — хранилище в памяти |
| `DATA_FILE` | пусто | путь к JSON-файлу для сохранения данных демо-режима (только без `DATABASE_URL`); секрет токенов при пустом `AUTH_SECRET` хранится в `<DATA_FILE>.secret` |
| `REDIS_URL` | пусто | строка подключения Redis; пусто — кэш в памяти |
| `AUTH_SECRET` | пусто | секрет подписи токенов; обязателен при заданном `DATABASE_URL`; при заданном `DATA_FILE` берётся из файла `<DATA_FILE>.secret` либо создаётся там |
| `TOKEN_TTL` | `72h` | срок жизни токена |
| `GEOIP_DB` | пусто | путь к базе MaxMind GeoLite2 (`.mmdb`) |
| `TRUST_PROXY` | `false` | доверять `X-Forwarded-For` и `CF-IPCountry` (только за обратным прокси) |
| `CODE_LENGTH` | `7` | длина генерируемого кода (4–32) |
| `ANALYTICS_WORKERS` | `4` | число воркеров аналитики |
| `ANALYTICS_BUFFER` | `10000` | размер очереди событий |
| `ANALYTICS_BATCH` | `500` | размер пакета записи |
| `ANALYTICS_FLUSH` | `1s` | интервал принудительного сброса пакета |
| `RATE_LIMIT_RPS` | `5` | лимит запросов в секунду на IP (создание ссылок, `/auth/*`) |
| `RATE_LIMIT_BURST` | `20` | всплеск для ограничения частоты |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TEST_DATABASE_URL` | пусто | только для интеграционных тестов |

Пример файла — [.env.example](.env.example).

## HTTP API

Префикс `/api/v1`, формат JSON, ошибки — `{"error": "текст"}`. Защищённые методы требуют
заголовок `Authorization: Bearer <token>`.

| Метод | Путь | Описание |
|---|---|---|
| POST | `/api/v1/auth/register` | регистрация, `201 {token, user}` |
| POST | `/api/v1/auth/login` | вход, `200 {token, user}` |
| GET | `/api/v1/me` | текущий пользователь |
| POST | `/api/v1/links` | создать ссылку, `201` |
| GET | `/api/v1/links?limit&offset` | список ссылок владельца |
| GET | `/api/v1/links/{code}` | одна ссылка |
| DELETE | `/api/v1/links/{code}` | удалить (деактивировать), `204` |
| GET | `/api/v1/links/{code}/stats?from&to` | статистика (даты `YYYY-MM-DD`) |
| GET | `/api/v1/top?n=10` | популярные коды (публично) |
| GET | `/{code}` | редирект `302` |
| GET | `/qr/{code}.png?size=N` | QR-код короткой ссылки (PNG, `size` 64–1024, по умолчанию 256); публично; `404`/`410`/`400` |
| GET | `/healthz`, `/metrics` | состояние и метрики |

Примеры:

```bash
# регистрация (в ответе поле token)
curl -s -X POST http://localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"user@example.com","password":"correct-horse-battery"}'

TOKEN=...   # значение поля token

# вход
curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"user@example.com","password":"correct-horse-battery"}'

# создание ссылки (alias, title, expires_at необязательны)
curl -s -X POST http://localhost:8080/api/v1/links \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/long/path","alias":"my-link","title":"Пример"}'

# редирект (без следования за Location)
curl -i http://localhost:8080/my-link

# список, одна ссылка, статистика, удаление
curl -s -H "Authorization: Bearer $TOKEN" 'http://localhost:8080/api/v1/links?limit=20'
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/links/my-link
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://localhost:8080/api/v1/links/my-link/stats?from=2026-01-01&to=2026-12-31'
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/links/my-link

# популярные ссылки
curl -s 'http://localhost:8080/api/v1/top?n=5'
```

## Структура каталогов

```
cmd/shortener/        точка входа сервиса
cmd/loadgen/          генератор нагрузки для проверки редиректа
internal/analytics/   сбор и обогащение событий (UA, реферер, GeoIP)
internal/auth/        регистрация, вход, токены
internal/cache/       кэш ссылок: Redis и память
internal/config/      конфигурация из окружения
internal/httpapi/     HTTP-слой: маршруты, middleware, rate limit, метрики
internal/model/       доменные типы
internal/qrcode/      генерация PNG с QR-кодом (rsc.io/qr + собственная отрисовка)
internal/service/     бизнес-логика (создание, разрешение, статистика)
internal/shortcode/   генерация и проверка кодов
internal/storage/     интерфейс хранилища; memory/ (в том числе снимок в файл) и postgres/ (миграции)
web/                  встроенный дашборд (HTML, CSS, JS)
docs/                 проектная документация, отчёт, k6-сценарий, скриншоты
.github/              CI и Dependabot
```

## Тестирование

```bash
# модульные тесты
go test ./...

# с детектором гонок (как в CI; нужен CGO)
go test -race ./...

# интеграционные тесты с PostgreSQL
export TEST_DATABASE_URL='postgres://shortener:shortener@localhost:5432/shortener?sslmode=disable'
go test -tags integration ./...

# покрытие
make cover

# микробенчмарки
go test -run='^$' -bench=. -benchmem ./internal/service/ ./internal/httpapi/
```

Нагрузочное тестирование редиректа (сервис должен быть запущен, `CODE` — код созданной ссылки):

```bash
go run ./cmd/loadgen -url http://localhost:8080/CODE -c 64 -d 10s
```

Флаги `loadgen`: `-url`, `-c` (конкурентность), `-d` (длительность), `-n` (число запросов
вместо времени). Выводятся RPS, статусы и перцентили задержки p50/p90/p95/p99/max.
Альтернатива — сценарий k6:

```bash
k6 run -e BASE_URL=http://localhost:8080 docs/loadtest/k6-redirect.js
```

Результаты замеров на стенде автора — в [docs/report/05-testing-data.md](docs/report/05-testing-data.md).

## Качество и безопасность

- `golangci-lint` (конфигурация — [.golangci.yml](.golangci.yml)): `make lint`.
- `gosec` и `govulncheck`: `make sec`.
- Dependabot следит за обновлениями зависимостей ([.github/dependabot.yml](.github/dependabot.yml)).
- CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)): `go vet`, тесты с `-race` и покрытием,
  интеграционные тесты на PostgreSQL, линтер, gosec, govulncheck, сборка Docker-образа.
- Пароли хранятся в виде bcrypt-хэшей, IP посетителей не сохраняется, у HTTP-сервера заданы таймауты,
  ссылки допускают только схемы `http` и `https`, ссылки на сам сервис запрещены.

## Скриншоты

| Вход | Дашборд (светлая тема) |
|---|---|
| ![Вход](docs/screenshots/01-login.png) | ![Дашборд, светлая тема](docs/screenshots/02-dashboard-light.png) |

| Дашборд (тёмная тема) | Мобильная версия |
|---|---|
| ![Дашборд, тёмная тема](docs/screenshots/03-dashboard-dark.png) | ![Мобильная версия](docs/screenshots/04-mobile.png) |

## Лицензия

[MIT](LICENSE), автор — Skufik2007, 2026.
