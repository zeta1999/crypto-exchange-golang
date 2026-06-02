package toxicity

import (
	"math"
	"testing"
)

func TestKyleLambdaPositiveImpact(t *testing.T) {
	k := newKyle(100)
	// A trade's price change (move since the previous trade) is paired with
	// that trade's signed volume: a buy lands at a higher price, a sell at a
	// lower one — a clean positive-impact relationship.
	price := 100.0
	k.observe(price, 1) // seed last price
	for i := 0; i < 50; i++ {
		price += 0.5
		k.observe(price, 1) // buy moved price up: dp>0, sv>0
		price -= 0.3
		k.observe(price, -1) // sell moved price down: dp<0, sv<0
	}
	if l := k.lambda(); l <= 0 {
		t.Errorf("lambda = %v, want > 0 for buy-pushes-up flow", l)
	}
}

func TestKyleLambdaZeroWhenNoFlow(t *testing.T) {
	k := newKyle(100)
	if k.lambda() != 0 {
		t.Error("empty lambda should be 0")
	}
	// Flat price → zero numerator → 0.
	for i := 0; i < 10; i++ {
		k.observe(100, 1)
	}
	if k.lambda() != 0 {
		t.Errorf("flat-price lambda = %v, want 0", k.lambda())
	}
}

func TestKyleWindowBounded(t *testing.T) {
	k := newKyle(5)
	for i := 0; i < 100; i++ {
		k.observe(float64(i), 1)
	}
	if len(k.dp) > 5 || len(k.sv) > 5 {
		t.Errorf("window not bounded: %d/%d", len(k.dp), len(k.sv))
	}
}

func TestVPINImbalance(t *testing.T) {
	// All-buy flow → maximal imbalance (1.0).
	v := newVPIN(10, 10)
	for i := 0; i < 100; i++ {
		v.observe(1, true)
	}
	if got := v.value(); math.Abs(got-1) > 1e-9 {
		t.Errorf("all-buy VPIN = %v, want 1", got)
	}

	// Balanced flow → ~0 imbalance.
	v2 := newVPIN(10, 10)
	for i := 0; i < 100; i++ {
		v2.observe(1, i%2 == 0)
	}
	if got := v2.value(); got > 0.3 {
		t.Errorf("balanced VPIN = %v, want near 0", got)
	}
}

func TestVPINEmptyAndDefaults(t *testing.T) {
	v := newVPIN(10, 10)
	if v.value() != 0 {
		t.Error("empty VPIN should be 0")
	}
}

func TestModelScoreAndScaleGate(t *testing.T) {
	m := New(Config{Scale: 1, VPINWeight: 1, KyleWeight: 1, BucketVolume: 10, Buckets: 10, WindowTrades: 100})
	// Drive imbalanced, impactful flow.
	price := 100.0
	for i := 0; i < 200; i++ {
		m.Observe(price, 1, true)
		price += 0.1
	}
	if m.VPIN() < 0.9 {
		t.Errorf("VPIN = %v, want high", m.VPIN())
	}
	if m.Score() <= 0 || m.Score() > 1 {
		t.Errorf("Score = %v, want (0,1]", m.Score())
	}
	if m.Lambda() <= 0 || m.Impact() <= 0 {
		t.Errorf("Impact = %v, want > 0", m.Impact())
	}
}

func TestModelScoreClampedAndWeights(t *testing.T) {
	// VPINWeight > 1 must still clamp Score to 1.
	m := New(Config{VPINWeight: 5, BucketVolume: 10, Buckets: 10})
	for i := 0; i < 100; i++ {
		m.Observe(100, 1, true)
	}
	if s := m.Score(); s != 1 {
		t.Errorf("Score with weight 5 = %v, want clamped to 1", s)
	}
}
