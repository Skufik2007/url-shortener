package httpapi

import (
	"math"
	"sync"
	"time"
)

// RateLimiter — ограничитель частоты по ключу (IP) на алгоритме «token bucket».
// Каждый ключ получает ведро ёмкостью burst, пополняемое rps токенов в секунду.
type RateLimiter struct {
	mu        sync.Mutex
	rps       float64
	burst     float64
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// sweepInterval — как часто чистим карту от давно не активных ключей.
const sweepInterval = time.Minute

// NewRateLimiter создаёт ограничитель. При rps <= 0 ограничение отключено.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &RateLimiter{
		rps:     rps,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Allow пытается списать один токен у ключа. Если токенов нет, возвращает
// false и время, через которое имеет смысл повторить запрос.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.rps <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rps)
			b.last = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rps * float64(time.Second))
	return false, wait
}

// sweep удаляет ведра, которые успели бы полностью наполниться: для клиента
// это неотличимо от нового ведра. Вызывается под мьютексом.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	refill := time.Duration(l.burst / l.rps * float64(time.Second))
	if refill < sweepInterval {
		refill = sweepInterval
	}
	for k, b := range l.buckets {
		if now.Sub(b.last) >= refill {
			delete(l.buckets, k)
		}
	}
}

// size возвращает число отслеживаемых ключей (для тестов).
func (l *RateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
