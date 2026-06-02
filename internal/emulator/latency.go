package emulator

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// LatencyConfig holds the four base delays of a Latency injector. All zero
// (the zero value) means every delay is 0 and Sleep is a no-op.
type LatencyConfig struct {
	FeedToBook time.Duration // delay applied before a book event reaches the reference
	OrderAck   time.Duration // delay before an order acknowledgement is returned (API edge)
	FillReport time.Duration // delay before a fill report is delivered (API edge)
	Jitter     time.Duration // upper bound of an added uniform random jitter [0, Jitter)
}

// Latency injects configurable delays so an OMS / strategy under test
// experiences a slow or racy venue (PLAN §5 Phase 7). Each base delay is
// returned with a seeded, concurrency-safe uniform jitter in [0, Jitter), so a
// scenario reproduces bit-for-bit across runs. The all-zero config yields zero
// delays and a no-op Sleep, satisfying the "zeroed controls are no-ops" DoD.
type Latency struct {
	cfg LatencyConfig

	mu  sync.Mutex
	rng *rand.Rand
}

// NewLatency builds a Latency from cfg, seeding its jitter RNG for
// reproducibility.
func NewLatency(cfg LatencyConfig, seed int64) *Latency {
	return &Latency{
		cfg: cfg,
		rng: rand.New(rand.NewSource(seed)),
	}
}

// delay returns base plus a uniform random jitter in [0, Jitter) (no jitter
// when Jitter <= 0). The RNG draw is guarded by a mutex so concurrent callers
// stay deterministic given a fixed seed.
func (l *Latency) delay(base time.Duration) time.Duration {
	if l.cfg.Jitter <= 0 {
		return base
	}
	l.mu.Lock()
	j := time.Duration(l.rng.Int63n(int64(l.cfg.Jitter)))
	l.mu.Unlock()
	return base + j
}

// FeedToBookDelay returns the jittered feed→book delay.
func (l *Latency) FeedToBookDelay() time.Duration { return l.delay(l.cfg.FeedToBook) }

// OrderAckDelay returns the jittered order-acknowledgement delay (API edge).
func (l *Latency) OrderAckDelay() time.Duration { return l.delay(l.cfg.OrderAck) }

// FillReportDelay returns the jittered fill-report delay (API edge).
func (l *Latency) FillReportDelay() time.Duration { return l.delay(l.cfg.FillReport) }

// Sleep blocks for d, returning early if ctx is cancelled. It is a no-op when
// d <= 0 (so an unconfigured latency adds nothing).
func (l *Latency) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
