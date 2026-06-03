package emulator

import (
	"testing"
	"time"
)

func TestParseLatencyDist(t *testing.T) {
	if ParseLatencyDist("poisson") != DistPoisson {
		t.Error("poisson not parsed")
	}
	if ParseLatencyDist("Poisson") != DistPoisson {
		t.Error("poisson not case-insensitive")
	}
	if ParseLatencyDist("") != DistUniform || ParseLatencyDist("uniform") != DistUniform {
		t.Error("default/uniform not parsed")
	}
}

// TestPoissonLatencyDeterministicAndMean verifies the shifted-Poisson latency is
// reproducible under a fixed seed and that its mean tracks base + Jitter.
func TestPoissonLatencyDeterministicAndMean(t *testing.T) {
	cfg := LatencyConfig{OrderAck: 5 * time.Millisecond, Jitter: 10 * time.Millisecond, Dist: DistPoisson}

	// Deterministic: same seed → identical sequence.
	l1, l2 := NewLatency(cfg, 42), NewLatency(cfg, 42)
	varied := false
	for i := 0; i < 200; i++ {
		a, b := l1.OrderAckDelay(), l2.OrderAckDelay()
		if a != b {
			t.Fatalf("poisson latency not deterministic at i=%d: %v vs %v", i, a, b)
		}
		if a != 5*time.Millisecond {
			varied = true // stochastic component is firing
		}
	}
	if !varied {
		t.Fatal("poisson jitter never deviated from the base — not stochastic")
	}

	// Mean over many draws ≈ base (5ms) + Poisson mean (10ms) = 15ms.
	l := NewLatency(cfg, 7)
	const n = 20000
	var sum time.Duration
	for i := 0; i < n; i++ {
		sum += l.OrderAckDelay()
	}
	mean := sum / n
	if mean < 13*time.Millisecond || mean > 17*time.Millisecond {
		t.Errorf("poisson mean delay = %v, want ~15ms", mean)
	}

	// Zero jitter → exactly the base, regardless of distribution.
	z := NewLatency(LatencyConfig{OrderAck: 5 * time.Millisecond, Dist: DistPoisson}, 1)
	if got := z.OrderAckDelay(); got != 5*time.Millisecond {
		t.Errorf("zero-jitter poisson delay = %v, want 5ms", got)
	}
}

// TestPoissonLargeLambdaNoHang guards the underflow hang: a large jitter (mean)
// must sample promptly via the normal approximation and stay near the mean.
func TestPoissonLargeLambdaNoHang(t *testing.T) {
	// lambda = 2000 (> the exp(-lambda) underflow threshold ~745).
	l := NewLatency(LatencyConfig{Jitter: 2 * time.Second, Dist: DistPoisson}, 3)
	done := make(chan time.Duration, 1)
	go func() {
		var sum time.Duration
		const n = 2000
		for i := 0; i < n; i++ {
			sum += l.OrderAckDelay()
		}
		done <- sum / n
	}()
	select {
	case mean := <-done:
		// mean ≈ 2000ms (base is 0 here); allow a wide band.
		if mean < 1800*time.Millisecond || mean > 2200*time.Millisecond {
			t.Errorf("large-lambda poisson mean = %v, want ~2s", mean)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poisson sampling hung on large lambda")
	}
}
