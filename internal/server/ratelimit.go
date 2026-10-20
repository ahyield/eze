package server

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter gives each user a token bucket for API calls, so one runaway loop on one
// site can't starve the shared server.
type rateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	buckets map[string]*bucket
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

func newRateLimiter(perSecond float64, burst int) *rateLimiter {
	rl := &rateLimiter{limit: rate.Limit(perSecond), burst: burst, buckets: map[string]*bucket{}}
	go rl.sweep()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{l: rate.NewLimiter(rl.limit, rl.burst)}
		rl.buckets[key] = b
	}
	b.seen = time.Now()
	rl.mu.Unlock()
	return b.l.Allow()
}

func (rl *rateLimiter) sweep() {
	for range time.Tick(time.Minute) {
		cutoff := time.Now().Add(-10 * time.Minute)
		rl.mu.Lock()
		for k, b := range rl.buckets {
			if b.seen.Before(cutoff) {
				delete(rl.buckets, k)
			}
		}
		rl.mu.Unlock()
	}
}
