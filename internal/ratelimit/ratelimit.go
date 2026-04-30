// Package ratelimit реализует простой in-memory token-bucket лимитер,
// ключуемый по IP клиента. Подходит для self-hosted MVP — не пытается
// синхронизироваться между инстансами и не персистится.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter — набор ключей (IP) → bucket.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	// rate — токенов в секунду; burst — максимум в bucket'е.
	rate  float64
	burst float64

	// Покрываем GC от заброшенных ключей.
	idleTTL time.Duration
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New создаёт лимитер: max за period (например, New(5, time.Minute, 5)
// = 5 запросов в минуту, burst 5).
func New(max int, period time.Duration, burst int) *Limiter {
	if max <= 0 {
		max = 1
	}
	if period <= 0 {
		period = time.Second
	}
	if burst <= 0 {
		burst = max
	}
	return &Limiter{
		buckets: map[string]*bucket{},
		rate:    float64(max) / period.Seconds(),
		burst:   float64(burst),
		idleTTL: period * 4,
		lastGC:  time.Now(),
	}
}

// Allow возвращает true, если для данного ключа найден свободный токен,
// иначе false (запрос надо отклонить).
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.lastGC) > l.idleTTL {
		for k, b := range l.buckets {
			if now.Sub(b.last) > l.idleTTL {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ClientIP пытается извлечь реальный IP клиента, учитывая X-Forwarded-For
// (берёт первый), затем X-Real-IP, затем r.RemoteAddr.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.IndexByte(xff, ','); comma >= 0 {
			xff = xff[:comma]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Wrap оборачивает HandlerFunc лимитом по IP. На превышении возвращает 429.
func (l *Limiter) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if !l.Allow(ip) {
			http.Error(w, "rate limit exceeded — попробуйте чуть позже", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
