package httpapi

import (
	"net"
	"net/http"
	"strings"
)

// clientIP определяет адрес клиента. X-Forwarded-For учитывается только
// при trustProxy (сервис стоит за доверенным обратным прокси); берётся
// первый адрес списка. Без доверия используется RemoteAddr.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
