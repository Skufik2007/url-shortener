package config

import (
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
