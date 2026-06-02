package replay

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// threeTrades returns a JSONL trace of three trades spaced 100ms apart.
func threeTrades() string {
	base := time.Unix(1_700_000_000, 0).UTC()
	var sb strings.Builder
	enc := NewRecorder(&sb)
	for i := 0; i < 3; i++ {
		_ = enc.Record(feed.Event{
			Kind:  feed.EventTrade,
			Trade: &feed.Trade{Instrument: "BTC-USD", Timestamp: base.Add(time.Duration(i) * 100 * time.Millisecond), Price: 100, Quantity: 1},
		})
	}
	return sb.String()
}

func drain(t *testing.T, src *Source) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	ch, err := src.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	n := 0
	for range ch {
		n++
	}
	return n, time.Since(start)
}

// TestReplayNoPacing confirms the default (speed<=0) is as-fast-as-possible:
// all events drain effectively instantly. This is the determinism guarantee
// the golden tests depend on.
func TestReplayNoPacing(t *testing.T) {
	n, elapsed := drain(t, NewReader(strings.NewReader(threeTrades())))
	if n != 3 {
		t.Fatalf("got %d events, want 3", n)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("unpaced replay took %v, expected near-instant", elapsed)
	}
}

// TestReplayPaced confirms WithSpeed paces by the recorded inter-event gaps.
// The trace spans 200ms; at 4x it should take ~50ms — clearly more than the
// unpaced case, but well under real time.
func TestReplayPaced(t *testing.T) {
	n, elapsed := drain(t, NewReader(strings.NewReader(threeTrades()), WithSpeed(4)))
	if n != 3 {
		t.Fatalf("got %d events, want 3", n)
	}
	// 200ms / 4 = 50ms expected. Allow generous bounds for scheduler jitter,
	// but it must be visibly paced (> 20ms) and not anywhere near real time.
	if elapsed < 20*time.Millisecond {
		t.Errorf("paced replay took %v, expected ~50ms (not instant)", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("paced replay took %v, far over the ~50ms target", elapsed)
	}
}

// TestReplayPacedCancel confirms a paced replay stops promptly on context
// cancellation (it must not sleep out the full schedule).
func TestReplayPacedCancel(t *testing.T) {
	src := NewReader(strings.NewReader(threeTrades()), WithSpeed(0.01)) // very slow → long sleeps
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-ch // first event is emitted immediately (anchor)
	cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("paced replay did not stop promptly after cancel")
	}
}
