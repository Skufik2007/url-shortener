// Package model содержит доменные сущности сервиса сокращения ссылок.
package model

import "time"

// User — владелец ссылок, работающий с дашбордом.
type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Link — короткая ссылка.
type Link struct {
	ID          int64      `json:"id"`
	Code        string     `json:"code"`
	OriginalURL string     `json:"original_url"`
	OwnerID     int64      `json:"owner_id"`
	Title       string     `json:"title,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	IsActive    bool       `json:"is_active"`
	ClickCount  int64      `json:"click_count"`
}

// Expired сообщает, истёк ли срок жизни ссылки на момент now.
func (l *Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
}

// Click — один переход по короткой ссылке (уже обогащённый).
type Click struct {
	ID        int64     `json:"id"`
	LinkID    int64     `json:"link_id"`
	ClickedAt time.Time `json:"clicked_at"`
	Referrer  string    `json:"referrer"` // домен источника или "direct"
	Country   string    `json:"country"`  // ISO-3166 alpha-2 или "ZZ" (неизвестно)
	Browser   string    `json:"browser"`
	OS        string    `json:"os"`
	Device    string    `json:"device"` // desktop | mobile | tablet | bot
	VisitorID string    `json:"-"`      // обезличенный хэш IP+UA для подсчёта уникальных
}

// DayCount — число переходов за день.
type DayCount struct {
	Day   time.Time `json:"day"`
	Count int64     `json:"count"`
}

// KeyCount — число переходов в разрезе одного значения измерения.
type KeyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// LinkStats — агрегированная статистика по ссылке за период.
type LinkStats struct {
	Code           string     `json:"code"`
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	TotalClicks    int64      `json:"total_clicks"`
	UniqueVisitors int64      `json:"unique_visitors"`
	ByDay          []DayCount `json:"by_day"`
	ByCountry      []KeyCount `json:"by_country"`
	ByReferrer     []KeyCount `json:"by_referrer"`
	ByBrowser      []KeyCount `json:"by_browser"`
	ByOS           []KeyCount `json:"by_os"`
	ByDevice       []KeyCount `json:"by_device"`
}
