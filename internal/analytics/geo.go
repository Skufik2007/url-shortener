package analytics

import (
	"net"
	"strings"

	"github.com/oschwald/maxminddb-golang"
)

// GeoResolver определяет страну по IP-адресу.
type GeoResolver interface {
	// Country возвращает код ISO-3166 alpha-2 в верхнем регистре или "".
	Country(ip string) string
}

// NoopGeo — заглушка, когда база GeoIP не задана.
type NoopGeo struct{}

// Country всегда возвращает пустую строку.
func (NoopGeo) Country(string) string { return "" }

// MaxMindGeo определяет страну по базе MaxMind GeoLite2-Country.
type MaxMindGeo struct {
	db *maxminddb.Reader
}

// OpenMaxMind открывает файл базы .mmdb.
func OpenMaxMind(path string) (*MaxMindGeo, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &MaxMindGeo{db: db}, nil
}

// Country ищет страну по IP; приватные, loopback и некорректные адреса дают "".
func (g *MaxMindGeo) Country(ipStr string) string {
	if g == nil || g.db == nil {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ""
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := g.db.Lookup(ip, &rec); err != nil {
		return ""
	}
	return strings.ToUpper(rec.Country.ISOCode)
}

// Close освобождает базу.
func (g *MaxMindGeo) Close() error {
	if g == nil || g.db == nil {
		return nil
	}
	return g.db.Close()
}
