// Package ratelimit is a hand-rolled, dependency-free token-bucket rate
// limiter (no golang.org/x/time/rate). A Limiter refills at a fixed rate up to
// a burst capacity; a KeyedLimiter maintains one bucket per key (API key or IP)
// with lazy creation and idle eviction so the map cannot grow unbounded.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a concurrency-safe token bucket. Tokens accrue at rate per second
// up to burst. A non-positive rate disables limiting (Allow always true).
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64 // bucket capacity
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewLimiter returns a limiter at rate tokens/sec with the given burst,
// starting full. rate <= 0 disables limiting.
func NewLimiter(rate float64, burst int) *Limiter {
	return newLimiterClock(rate, burst, time.Now)
}

func newLimiterClock(rate float64, burst int, now func() time.Time) *Limiter {
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	return &Limiter{rate: rate, burst: b, tokens: b, last: now(), now: now}
}

// Allow reports whether one token is available, consuming it if so.
func (l *Limiter) Allow() bool { return l.AllowN(1) }

// AllowN reports whether n tokens are available, consuming them if so.
func (l *Limiter) AllowN(n int) bool {
	if l.rate <= 0 {
		return true // disabled
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	need := float64(n)
	if l.tokens >= need {
		l.tokens -= need
		return true
	}
	return false
}

// KeyedLimiter maps a key (API key / IP) to its own Limiter, creating buckets
// lazily and evicting ones idle longer than ttl to bound memory.
type KeyedLimiter struct {
	rate  float64
	burst int
	ttl   time.Duration
	now   func() time.Time

	maxKeys int

	mu      sync.Mutex
	buckets map[string]*keyedBucket
	lastGC  time.Time
}

// defaultMaxKeys hard-caps live per-key buckets, so a flood of distinct
// keys/IPs within one ttl window (before the periodic sweep runs) cannot grow
// the map unboundedly.
const defaultMaxKeys = 100_000

type keyedBucket struct {
	lim  *Limiter
	seen time.Time
}

// NewKeyedLimiter returns a keyed limiter. rate <= 0 disables limiting for all
// keys. Idle buckets are evicted after ttl (a zero ttl defaults to 10 minutes).
func NewKeyedLimiter(rate float64, burst int, ttl time.Duration) *KeyedLimiter {
	return newKeyedLimiterClock(rate, burst, ttl, time.Now)
}

func newKeyedLimiterClock(rate float64, burst int, ttl time.Duration, now func() time.Time) *KeyedLimiter {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &KeyedLimiter{
		rate:    rate,
		burst:   burst,
		ttl:     ttl,
		maxKeys: defaultMaxKeys,
		now:     now,
		buckets: make(map[string]*keyedBucket),
		lastGC:  now(),
	}
}

// Allow reports whether the bucket for key permits one request.
func (k *KeyedLimiter) Allow(key string) bool {
	if k.rate <= 0 {
		return true // disabled — no bucket allocation
	}
	k.mu.Lock()
	now := k.now()
	b, ok := k.buckets[key]
	if !ok {
		// Enforce the hard cap before inserting a new bucket: sweep idle ones
		// (ignoring the periodic interval), then if still full evict the
		// least-recently-seen, so the map can never exceed maxKeys.
		if k.maxKeys > 0 && len(k.buckets) >= k.maxKeys {
			k.sweepLocked(now)
			if len(k.buckets) >= k.maxKeys {
				k.evictOldestLocked()
			}
		}
		b = &keyedBucket{lim: newLimiterClock(k.rate, k.burst, k.now)}
		k.buckets[key] = b
	}
	b.seen = now
	k.gcLocked(now)
	lim := b.lim
	k.mu.Unlock()
	return lim.Allow()
}

// gcLocked evicts idle buckets at most once per ttl. Caller holds k.mu.
func (k *KeyedLimiter) gcLocked(now time.Time) {
	if now.Sub(k.lastGC) < k.ttl {
		return
	}
	k.sweepLocked(now)
}

// sweepLocked evicts buckets idle longer than ttl. Caller holds k.mu.
func (k *KeyedLimiter) sweepLocked(now time.Time) {
	k.lastGC = now
	for key, b := range k.buckets {
		if now.Sub(b.seen) > k.ttl {
			delete(k.buckets, key)
		}
	}
}

// evictOldestLocked removes the single least-recently-seen bucket, to keep the
// map at the hard cap when a sweep didn't free anything. Caller holds k.mu.
func (k *KeyedLimiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for key, b := range k.buckets {
		if first || b.seen.Before(oldest) {
			oldestKey, oldest, first = key, b.seen, false
		}
	}
	if !first {
		delete(k.buckets, oldestKey)
	}
}

// Len returns the current number of live buckets (for tests/observability).
func (k *KeyedLimiter) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.buckets)
}
