// Package shortcode генерирует и проверяет короткие коды ссылок.
package shortcode

// Generator выдаёт новый случайный код.
type Generator interface {
	Generate() (string, error)
}

// Контракт пакета (реализация — в generator.go):
//
//   - NewRandom(length int) *Random — генератор base62 (0-9A-Za-z) на crypto/rand,
//     без смещения распределения (rejection sampling). Длина по умолчанию 7.
//   - ValidateAlias(s string) error — пользовательский алиас: 3..32 символа
//     [A-Za-z0-9_-], не начинается с '-' или '_', не входит в Reserved (без учёта регистра).
//   - LooksValid(s string) bool — быстрая проверка кода из URL: 1..32 символа [A-Za-z0-9_-].
//   - Reserved — зарезервированные пути: api, dashboard, static, healthz, metrics,
//     login, register, admin, favicon.ico, robots.txt.
