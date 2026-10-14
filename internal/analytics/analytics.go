// Package analytics собирает переходы по ссылкам асинхронно:
// HTTP-обработчик кладёт событие в буферизованный канал, пул горутин
// обогащает его (гео, браузер, реферер) и пакетно пишет в хранилище.
package analytics

import "time"

// Event — сырое событие перехода, снятое с HTTP-запроса.
type Event struct {
	LinkID      int64
	Time        time.Time
	IP          string // адрес клиента (с учётом доверенного X-Forwarded-For)
	UserAgent   string
	Referer     string
	CountryHint string // страна из заголовка CDN (CF-IPCountry), если есть
}

// Tracker принимает события; реализация не должна блокировать вызывающего.
type Tracker interface {
	Track(e Event)
}
