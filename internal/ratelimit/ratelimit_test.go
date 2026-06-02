package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is an injectable monotonic clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestLimiterBurstThenThrottle(t *testing.T) {
	clk := newFakeClock()
	l := newLimiterClock(10, 5, clk.Now) // 10/s, burst 5
	for i := 0; i < 5; i++ {
		if !l.Allow() {
			t.Fatalf("burst token %d denied", i)
		}
	}
	if l.Allow() {
		t.Fatal("6th request should be throttled (burst exhausted)")
	}
}

func TestLimiterRefill(t *testing.T) {
	clk := newFakeClock()
	l := newLimiterClock(10, 5, clk.Now) // 10/s
	for i := 0; i < 5; i++ {
		l.Allow()
	}
	if l.Allow() {
		t.Fatal("should be empty")
	}
	clk.Advance(200 * time.Millisecond) // +2 tokens
	if !l.Allow() || !l.Allow() {
		t.Fatal("expected 2 refilled tokens")
	}
	if l.Allow() {
		t.Fatal("only 2 tokens should have refilled")
	}
}

func TestLimiterDisabled(t *testing.T) {
	l := NewLimiter(0, 1)
	for i := 0; i < 1000; i++ {
		if !l.Allow() {
			t.Fatal("disabled limiter should allow all")
		}
	}
}

func TestKeyedIsolation(t *testing.T) {
	clk := newFakeClock()
	k := newKeyedLimiterClock(10, 2, time.Minute, clk.Now)
	if !k.Allow("a") || !k.Allow("a") {
		t.Fatal("key a burst denied")
	}
	if k.Allow("a") {
		t.Fatal("key a should be exhausted")
	}
	// Different key has its own fresh bucket.
	if !k.Allow("b") || !k.Allow("b") {
		t.Fatal("key b should have its own bucket")
	}
}

func TestKeyedEviction(t *testing.T) {
	clk := newFakeClock()
	k := newKeyedLimiterClock(10, 2, time.Minute, clk.Now)
	k.Allow("a")
	if k.Len() != 1 {
		t.Fatalf("len = %d, want 1", k.Len())
	}
	clk.Advance(2 * time.Minute) // a goes idle
	k.Allow("b")                 // triggers GC
	if k.Len() != 1 {
		t.Fatalf("idle bucket not evicted, len = %d", k.Len())
	}
}

func TestKeyedDisabled(t *testing.T) {
	k := NewKeyedLimiter(0, 1, time.Minute)
	for i := 0; i < 100; i++ {
		if !k.Allow("x") {
			t.Fatal("disabled keyed limiter should allow all")
		}
	}
	if k.Len() != 0 {
		t.Fatalf("disabled limiter should allocate no buckets, len=%d", k.Len())
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := NewLimiter(1000, 100)
	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if l.Allow() {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}()
	}
	wg.Wait()
	if allowed < 1 {
		t.Fatal("expected some allowed under concurrency")
	}
}
