package mcp

import (
	"sync"
	"time"
)

// limiter is a sliding-window limiter with a per-key cap and an overall cap.
// An attempt is recorded only when both caps allow it.
type limiter struct {
	mu      sync.Mutex
	perKey  int
	overall int
	window  time.Duration
	now     func() time.Time
	keys    map[string][]time.Time
	all     []time.Time
}

func newLimiter(perKey, overall int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{perKey: perKey, overall: overall, window: window, now: now, keys: map[string][]time.Time{}}
}

// allow records an attempt for key. When denied it returns the wait until
// the earliest slot frees.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	l.all = prune(l.all, cut)
	ks := prune(l.keys[key], cut)
	if len(ks) >= l.perKey {
		l.keys[key] = ks
		return false, ks[0].Add(l.window).Sub(now)
	}
	if len(l.all) >= l.overall {
		l.keys[key] = ks
		return false, l.all[0].Add(l.window).Sub(now)
	}
	l.keys[key] = append(ks, now)
	l.all = append(l.all, now)
	if len(l.keys) > 1000 {
		for k, v := range l.keys {
			if v = prune(v, cut); len(v) == 0 {
				delete(l.keys, k)
			} else {
				l.keys[k] = v
			}
		}
	}
	return true, 0
}

// prune drops timestamps at or before cut, reusing the backing array.
func prune(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}
