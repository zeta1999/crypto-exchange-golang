package emulator

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// LatencyDist selects how the stochastic component of a delay is drawn.
type LatencyDist int

const (
	// DistUniform adds a uniform jitter in [0, Jitter) (the default).
	DistUniform LatencyDist = iota
	// DistPoisson adds a Poisson-distributed delay with mean = Jitter, quantised
	// to milliseconds — a shifted-Poisson model: a fixed base delay plus a
	// Poisson stochastic component (bursty tail, unlike bounded uniform jitter).
	DistPoisson
)

// ParseLatencyDist maps a config string to a LatencyDist ("" / "uniform" →
// uniform; "poisson" → Poisson).
func ParseLatencyDist(s string) LatencyDist {
	if strings.EqualFold(s, "poisson") {
		return DistPoisson
	}
	return DistUniform
}

// LatencyConfig holds the four base delays of a Latency injector. All zero
// (the zero value) means every delay is 0 and Sleep is a no-op.
type LatencyConfig struct {
	FeedToBook time.Duration // delay applied before a book event reaches the reference
	OrderAck   time.Duration // delay before an order acknowledgement is returned (API edge)
	FillReport time.Duration // delay before a fill report is delivered (API edge)
	Jitter     time.Duration // uniform: upper bound [0, Jitter); poisson: mean of the added delay
	Dist       LatencyDist   // distribution of the jittered component (default uniform)
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

// delay returns base plus a jittered component drawn from the configured
// distribution (no jitter when Jitter <= 0). The RNG draw is guarded by a mutex
// so concurrent callers stay deterministic given a fixed seed.
func (l *Latency) delay(base time.Duration) time.Duration {
	if l.cfg.Jitter <= 0 {
		return base
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.cfg.Dist {
	case DistPoisson:
		meanMs := float64(l.cfg.Jitter) / float64(time.Millisecond)
		k := poissonSample(l.rng, meanMs)
		return base + time.Duration(k)*time.Millisecond
	default:
		return base + time.Duration(l.rng.Int63n(int64(l.cfg.Jitter)))
	}
}

// poissonSample draws a Poisson(lambda) count. For small means it uses Knuth's
// exact algorithm; for large means (> 30) it uses a normal approximation —
// which both avoids Knuth's O(lambda) loop AND the hang where exp(-lambda)
// underflows to 0 (lambda ≳ 745) and the loop can no longer terminate. Both
// branches draw from rng so a fixed seed stays reproducible. Returns 0 for
// lambda <= 0; never negative.
func poissonSample(rng *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		v := math.Round(lambda + math.Sqrt(lambda)*rng.NormFloat64())
		if v < 0 {
			v = 0
		}
		return int(v)
	}
	target := math.Exp(-lambda)
	k, p := 0, 1.0
	for {
		k++
		p *= rng.Float64()
		if p <= target {
			return k - 1
		}
	}
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
