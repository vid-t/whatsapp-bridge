package auth

import (
	"sync"
	"time"
)

// Limiter is a small in-memory sliding-window rate limiter keyed by string
// (an IP, a key id). It is good enough for one process; a shared store is
// not needed at this scale.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	now     func() time.Time
}

// NewLimiter creates a limiter.
func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string][]time.Time{}, now: time.Now}
}

// Allow records an attempt for key and reports whether it is within limit
// per window. When denied it also returns the wait until a slot frees.
func (l *Limiter) Allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-window)
	b := l.buckets[key][:0]
	for _, t := range l.buckets[key] {
		if t.After(cut) {
			b = append(b, t)
		}
	}
	if len(b) >= limit {
		l.buckets[key] = b
		return false, b[0].Add(window).Sub(now)
	}
	l.buckets[key] = append(b, now)
	if len(l.buckets) > 10000 {
		l.sweepLocked(cut)
	}
	return true, 0
}

func (l *Limiter) sweepLocked(cut time.Time) {
	for k, b := range l.buckets {
		if len(b) == 0 || !b[len(b)-1].After(cut) {
			delete(l.buckets, k)
		}
	}
}
