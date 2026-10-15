package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultsDemoMode(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.BaseURL != "http://localhost:8080" || c.CodeLength != 7 ||
		c.TokenTTL != 72*time.Hour || c.RateLimitRPS != 5 || c.RateLimitBurst != 20 {
		t.Fatalf("неожиданные значения по умолчанию: %+v", c)
	}
	if !c.GeneratedSecret || len(c.AuthSecret) < 32 {
		t.Fatalf("ожидался сгенерированный секрет, получено %+v", c)
	}
}

func TestSecretRequiredWithDatabase(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err == nil || !strings.Contains(err.Error(), "AUTH_SECRET") {
		t.Fatalf("ожидалась ошибка про AUTH_SECRET, получено %v", err)
	}
	_, err = LoadFrom(env(map[string]string{"DATABASE_URL": "postgres://x", "AUTH_SECRET": "short"}))
	if err == nil {
		t.Fatal("короткий секрет должен отклоняться")
	}
	c, err := LoadFrom(env(map[string]string{"DATABASE_URL": "postgres://x", "AUTH_SECRET": strings.Repeat("s", 32)}))
	if err != nil || c.GeneratedSecret {
		t.Fatalf("err=%v generated=%v", err, c.GeneratedSecret)
	}
}

func TestOverridesAndTrim(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"BASE_URL": "https://sho.rt/", "TRUST_PROXY": "true", "CODE_LENGTH": "9",
		"ANALYTICS_FLUSH": "250ms", "RATE_LIMIT_RPS": "2.5", "LOG_LEVEL": "DEBUG",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://sho.rt" || !c.TrustProxy || c.CodeLength != 9 ||
		c.AnalyticsFlush != 250*time.Millisecond || c.RateLimitRPS != 2.5 || c.LogLevel != "debug" {
		t.Fatalf("%+v", c)
	}
}

func TestInvalidValues(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"CODE_LENGTH": "abc", "TOKEN_TTL": "-1h", "BASE_URL": "nope", "TRUST_PROXY": "maybe", "LOG_LEVEL": "loud",
	}))
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	for _, k := range []string{"CODE_LENGTH", "TOKEN_TTL", "BASE_URL", "TRUST_PROXY", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("в ошибке нет %s: %v", k, err)
		}
	}
}

// DATA_FILE без AUTH_SECRET: секрет создаётся в файле один раз и переживает перезапуск.
func TestDataFileSecretPersists(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "data.json")
	c1, err := LoadFrom(env(map[string]string{"DATA_FILE": dataFile}))
	if err != nil {
		t.Fatal(err)
	}
	if c1.DataFile != dataFile || c1.SecretFile != dataFile+".secret" || c1.GeneratedSecret {
		t.Fatalf("неожиданная конфигурация: %+v", c1)
	}
	if len(c1.AuthSecret) < minSecretLen {
		t.Fatalf("секрет слишком короткий: %d", len(c1.AuthSecret))
	}
	info, err := os.Stat(c1.SecretFile)
	if err != nil {
		t.Fatalf("файл секрета не создан: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("файл секрета пуст")
	}

	c2, err := LoadFrom(env(map[string]string{"DATA_FILE": dataFile}))
	if err != nil {
		t.Fatal(err)
	}
	if c2.AuthSecret != c1.AuthSecret {
		t.Fatal("после «перезапуска» секрет изменился — токены перестали бы действовать")
	}

	// Явный AUTH_SECRET важнее файла.
	explicit := strings.Repeat("e", 40)
	c3, err := LoadFrom(env(map[string]string{"DATA_FILE": dataFile, "AUTH_SECRET": explicit}))
	if err != nil {
		t.Fatal(err)
	}
	if c3.AuthSecret != explicit || c3.SecretFile != "" {
		t.Fatalf("AUTH_SECRET должен иметь приоритет: %+v", c3)
	}
}

func TestDataFileErrors(t *testing.T) {
	dir := t.TempDir()

	// DATA_FILE вместе с DATABASE_URL — противоречивая настройка.
	_, err := LoadFrom(env(map[string]string{
		"DATA_FILE": filepath.Join(dir, "d.json"), "DATABASE_URL": "postgres://x", "AUTH_SECRET": strings.Repeat("s", 32),
	}))
	if err == nil || !strings.Contains(err.Error(), "DATA_FILE") {
		t.Fatalf("ожидалась ошибка про DATA_FILE, получено %v", err)
	}

	// Слишком короткий секрет в файле.
	short := filepath.Join(dir, "short.json")
	if err := os.WriteFile(short+".secret", []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(env(map[string]string{"DATA_FILE": short})); err == nil || !strings.Contains(err.Error(), "короткий") {
		t.Fatalf("ожидалась ошибка короткого секрета, получено %v", err)
	}

	// Каталога нет — секрет записать нельзя.
	if _, err := LoadOrCreateSecret(filepath.Join(dir, "missing", "x.secret")); err == nil {
		t.Fatal("ожидалась ошибка записи секрета")
	}
}
