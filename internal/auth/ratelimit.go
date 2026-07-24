package auth

import (
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket for login attempts.
// ponytail: unbounded map growth is capped by periodic sweep in Allow.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
	lastGC   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(capacity int, refillPerMinute float64) *RateLimiter {
	return &RateLimiter{
		buckets:  map[string]*bucket{},
		capacity: float64(capacity),
		refill:   refillPerMinute / 60,
		lastGC:   time.Now(),
	}
}

func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if now.Sub(r.lastGC) > 10*time.Minute {
		for k, b := range r.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(r.buckets, k)
			}
		}
		r.lastGC = now
	}

	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.capacity, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.capacity, b.tokens+now.Sub(b.last).Seconds()*r.refill)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
