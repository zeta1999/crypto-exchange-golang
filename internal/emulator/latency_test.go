package emulator

import (
	"context"
	"testing"
	"time"
)

func TestLatencyZeroConfig(t *testing.T) {
	l := NewLatency(LatencyConfig{}, 1)
	if d := l.FeedToBookDelay(); d != 0 {
		t.Fatalf("FeedToBookDelay = %v want 0", d)
	}
	if d := l.OrderAckDelay(); d != 0 {
		t.Fatalf("OrderAckDelay = %v want 0", d)
	}
	if d := l.FillReportDelay(); d != 0 {
		t.Fatalf("FillReportDelay = %v want 0", d)
	}

	// Sleep with zero duration returns immediately.
	start := time.Now()
	l.Sleep(context.Background(), l.FeedToBookDelay())
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("zero Sleep took %v", elapsed)
	}
}

func TestLatencyJitteredBounds(t *testing.T) {
	base := 10 * time.Millisecond
	jitter := 5 * time.Millisecond
	l := NewLatency(LatencyConfig{FeedToBook: base, OrderAck: base, FillReport: base, Jitter: jitter}, 42)

	for i := 0; i < 1000; i++ {
		d := l.FeedToBookDelay()
		if d < base || d >= base+jitter {
			t.Fatalf("FeedToBookDelay %v out of [%v, %v)", d, base, base+jitter)
		}
	}
}

func TestLatencyDeterministic(t *testing.T) {
	cfg := LatencyConfig{FeedToBook: 10 * time.Millisecond, Jitter: 5 * time.Millisecond}
	a := NewLatency(cfg, 7)
	b := NewLatency(cfg, 7)
	for i := 0; i < 50; i++ {
		if da, db := a.FeedToBookDelay(), b.FeedToBookDelay(); da != db {
			t.Fatalf("non-deterministic at %d: %v != %v", i, da, db)
		}
	}
}

func TestLatencyNoJitterIsBase(t *testing.T) {
	base := 8 * time.Millisecond
	l := NewLatency(LatencyConfig{OrderAck: base}, 1)
	if d := l.OrderAckDelay(); d != base {
		t.Fatalf("no-jitter delay = %v want %v", d, base)
	}
}

func TestLatencySleepRespectsContext(t *testing.T) {
	l := NewLatency(LatencyConfig{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	l.Sleep(ctx, time.Hour) // would block forever without ctx
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Sleep ignored cancelled ctx, took %v", elapsed)
	}
}
