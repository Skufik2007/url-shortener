package httpapi

import (
	"fmt"
	"io"
	"sync/atomic"
)

// CollectorStats — снимок счётчиков сборщика аналитики.
// Тип свой, чтобы пакет не зависел от реализации analytics.Collector.
type CollectorStats struct {
	Accepted, Dropped, Written, Failed uint64
}

// Metrics — набор атомарных счётчиков сервиса.
type Metrics struct {
	Requests        atomic.Uint64 // все HTTP-запросы
	Responses5xx    atomic.Uint64 // ответы с кодом 5xx
	Redirects       atomic.Uint64 // успешные редиректы по короткой ссылке
	RedirectMissing atomic.Uint64 // редиректы: ссылка не найдена
	RedirectGone    atomic.Uint64 // редиректы: ссылка истекла или удалена
	RateLimited     atomic.Uint64 // отклонено ограничителем частоты
	LinksCreated    atomic.Uint64
	InFlight        atomic.Int64 // запросов в обработке сейчас
}

// NewMetrics создаёт пустой набор счётчиков.
func NewMetrics() *Metrics { return &Metrics{} }

// WritePrometheus выводит счётчики в текстовом формате Prometheus.
// stats может быть nil.
// Ошибки записи игнорируются намеренно: они означают, что клиент закрыл соединение.
func (m *Metrics) WritePrometheus(w io.Writer, stats func() CollectorStats) {
	counter := func(name, help string, v uint64) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("shortener_http_requests_total", "Всего HTTP-запросов.", m.Requests.Load())
	counter("shortener_http_responses_5xx_total", "Ответы с ошибкой сервера.", m.Responses5xx.Load())
	counter("shortener_redirects_total", "Успешные редиректы.", m.Redirects.Load())
	counter("shortener_redirects_not_found_total", "Переходы по несуществующему коду.", m.RedirectMissing.Load())
	counter("shortener_redirects_gone_total", "Переходы по истёкшей или удалённой ссылке.", m.RedirectGone.Load())
	counter("shortener_rate_limited_total", "Запросы, отклонённые ограничителем частоты.", m.RateLimited.Load())
	counter("shortener_links_created_total", "Созданные ссылки.", m.LinksCreated.Load())
	_, _ = fmt.Fprintf(w, "# HELP shortener_http_in_flight Запросов в обработке.\n# TYPE shortener_http_in_flight gauge\nshortener_http_in_flight %d\n", m.InFlight.Load())

	if stats != nil {
		s := stats()
		counter("shortener_analytics_accepted_total", "События, принятые в очередь аналитики.", s.Accepted)
		counter("shortener_analytics_dropped_total", "События, отброшенные при переполнении очереди.", s.Dropped)
		counter("shortener_analytics_written_total", "События, записанные в хранилище.", s.Written)
		counter("shortener_analytics_failed_total", "События, которые не удалось записать.", s.Failed)
	}
}
