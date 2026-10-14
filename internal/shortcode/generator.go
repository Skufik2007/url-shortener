package shortcode

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	alphabet      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	defaultLength = 7
	minAliasLen   = 3
	maxAliasLen   = 32
	// maxByte — наибольшее кратное 62 значение, не превышающее 256:
	// байты >= maxByte отбрасываются (rejection sampling), чтобы не было смещения.
	maxByte = 256 - 256%len(alphabet) // 248
)

// Reserved — зарезервированные пути, которые нельзя занять алиасом.
var Reserved = map[string]struct{}{
	"api":         {},
	"dashboard":   {},
	"static":      {},
	"healthz":     {},
	"metrics":     {},
	"login":       {},
	"register":    {},
	"admin":       {},
	"favicon.ico": {},
	"robots.txt":  {},
}

// Ошибки валидации алиаса.
var (
	ErrAliasLength   = errors.New("алиас должен содержать от 3 до 32 символов")
	ErrAliasChars    = errors.New("алиас может содержать только латинские буквы, цифры, '-' и '_'")
	ErrAliasStart    = errors.New("алиас не может начинаться с '-' или '_'")
	ErrAliasReserved = errors.New("этот алиас зарезервирован")
)

// Random — генератор случайных base62-кодов на crypto/rand.
type Random struct {
	length int
}

var _ Generator = (*Random)(nil)

// NewRandom создаёт генератор кодов заданной длины (length <= 0 — 7 символов).
func NewRandom(length int) *Random {
	if length <= 0 {
		length = defaultLength
	}
	return &Random{length: length}
}

// Generate возвращает новый случайный код.
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

func isCodeChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '_'
}

// ValidateAlias проверяет пользовательский алиас.
func ValidateAlias(s string) error {
	if len(s) < minAliasLen || len(s) > maxAliasLen {
		return ErrAliasLength
	}
	for i := 0; i < len(s); i++ {
		if !isCodeChar(s[i]) {
			return ErrAliasChars
		}
	}
	if s[0] == '-' || s[0] == '_' {
		return ErrAliasStart
	}
	if _, ok := Reserved[strings.ToLower(s)]; ok {
		return ErrAliasReserved
	}
	return nil
}

// LooksValid — быстрая проверка кода из URL: 1..32 символа [A-Za-z0-9_-].
func LooksValid(s string) bool {
	if len(s) < 1 || len(s) > maxAliasLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isCodeChar(s[i]) {
			return false
		}
	}
	return true
}
