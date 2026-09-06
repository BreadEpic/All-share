package signal

import (
	"sync"
	"time"
)

// bucket is a token bucket with lazy refill.
//
// Rate limiting here is a security control, not a politeness measure: it is
// what keeps an attacker from grinding through pairing handles or hammering an
// agent with connection attempts. Limits are applied per source address *and*
// per identity, so neither a single host nor a single stolen key can dominate.
type bucket struct {
	tokens   float64
	last     time.Time
	capacity float64
	refill   float64 // tokens per second
}

func (b *bucket) allow(now time.Time, cost float64) bool {
	if b.last.IsZero() {
		b.last = now
		b.tokens = b.capacity
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// Limiter is a keyed collection of token buckets.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64
	lastGC   time.Time
	now      func() time.Time
}

// NewLimiter builds a limiter allowing burst operations, refilling at perSecond.
func NewLimiter(burst int, perSecond float64) *Limiter {
	return &Limiter{
		buckets:  map[string]*bucket{},
		capacity: float64(burst),
		refill:   perSecond,
		now:      time.Now,
	}
}

// Allow consumes one token for key, reporting whether it was available.
func (l *Limiter) Allow(key string) bool { return l.AllowN(key, 1) }

// AllowN consumes cost tokens for key.
func (l *Limiter) AllowN(key string, cost float64) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Periodically drop buckets that have refilled to full, so an attacker
	// cycling through source addresses cannot grow this map without bound.
	if now.Sub(l.lastGC) > time.Minute {
		l.lastGC = now
		for k, b := range l.buckets {
			if now.Sub(b.last) > 5*time.Minute {
				delete(l.buckets, k)
			}
		}
	}

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 50_000 {
			// Under a pathological flood, fail closed rather than exhaust memory.
			return false
		}
		b = &bucket{capacity: l.capacity, refill: l.refill}
		l.buckets[key] = b
	}
	return b.allow(now, cost)
}

// Size reports the number of live buckets, for metrics and tests.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
