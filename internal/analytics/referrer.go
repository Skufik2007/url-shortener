package analytics

import (
	"net/url"
	"strings"
)

// DirectReferrer — значение для переходов без внешнего источника.
const DirectReferrer = "direct"

// ReferrerDomain извлекает домен источника из заголовка Referer.
// Пустое или некорректное значение и собственный хост дают "direct";
// иначе возвращается имя хоста в нижнем регистре без префикса "www.".
func ReferrerDomain(ref, selfHost string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DirectReferrer
	}
	u, err := url.Parse(ref)
	if err != nil || u.Hostname() == "" {
		return DirectReferrer
	}
	host := normHost(u.Hostname())
	self := selfHost
	// selfHost может быть задан как host:port или даже как URL.
	if strings.Contains(self, "://") {
		if su, err := url.Parse(self); err == nil {
			self = su.Host
		}
	}
	if h, _, ok := strings.Cut(self, ":"); ok && !strings.Contains(self, "]") {
		self = h
	} else if su, err := url.Parse("//" + self); err == nil && su.Hostname() != "" {
		self = su.Hostname()
	}
	if self = normHost(self); self != "" && host == self {
		return DirectReferrer
	}
	return host
}

func normHost(h string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "www.")
}
