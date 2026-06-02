package emulator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

func mustParse(t *testing.T, jsonl string) *Scenario {
	t.Helper()
	sc, err := ParseScenario(strings.NewReader(jsonl), 42)
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
	return sc
}

func TestParseScenarioValidSortsAndSkipsComments(t *testing.T) {
	jsonl := `# a comment line, skipped
{"at_ms": 10000, "action": "price_shift", "params": {"offset_bps": 0}}

   # indented comment + blank line above
{"at_ms": 0, "action": "price_shift", "params": {"offset_bps": 15, "scale": 1.0}}
{"at_ms": 5000, "action": "latency", "params": {"feed_to_book_ms": 50, "jitter_ms": 10}}
`
	sc := mustParse(t, jsonl)
	if sc.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", sc.Len())
	}
	wantAt := []int64{0, 5000, 10000}
	for i, w := range wantAt {
		if sc.events[i].AtMs != w {
			t.Errorf("event %d at_ms = %d, want %d (not sorted)", i, sc.events[i].AtMs, w)
		}
	}
	// First event decoded as a 15bps shift.
	if got := sc.events[0].priceShift.OffsetBps; got != 15 {
		t.Errorf("first event offset_bps = %v, want 15", got)
	}
	// Latency event decoded.
	if got := sc.events[1].latency.FeedToBook; got != 50*time.Millisecond {
		t.Errorf("latency feed_to_book = %v, want 50ms", got)
	}
}

func TestParseScenarioStableSort(t *testing.T) {
	// Two events at the same at_ms must keep source order (stable).
	jsonl := `{"at_ms": 100, "action": "price_shift", "params": {"offset_bps": 1}}
{"at_ms": 100, "action": "price_shift", "params": {"offset_bps": 2}}
`
	sc := mustParse(t, jsonl)
	if sc.events[0].priceShift.OffsetBps != 1 || sc.events[1].priceShift.OffsetBps != 2 {
		t.Fatalf("stable sort broken: got %v then %v", sc.events[0].priceShift.OffsetBps, sc.events[1].priceShift.OffsetBps)
	}
}

func TestParseScenarioErrors(t *testing.T) {
	cases := []struct {
		name       string
		jsonl      string
		wantLine   string
		wantPhrase string
	}{
		{"unknown action", `{"at_ms": 0, "action": "explode", "params": {}}`, "line 1", "unknown action"},
		{"negative at_ms", `{"at_ms": -5, "action": "price_shift", "params": {}}`, "line 1", "at_ms must be >= 0"},
		{"malformed json", "{not json}", "line 1", "malformed JSON"},
		{"missing action", `{"at_ms": 0, "params": {}}`, "line 1", "missing action"},
		{"unknown param", `{"at_ms": 0, "action": "price_shift", "params": {"bogus": 1}}`, "line 1", "params"},
		{"negative latency", `{"at_ms": 0, "action": "latency", "params": {"feed_to_book_ms": -1}}`, "line 1", ">= 0"},
		{"error reports later line", "{\"at_ms\":0,\"action\":\"price_shift\",\"params\":{}}\n{bad}", "line 2", "malformed JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScenario(strings.NewReader(tc.jsonl), 0)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantLine) {
				t.Errorf("error %q missing %q", err.Error(), tc.wantLine)
			}
			if !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Errorf("error %q missing phrase %q", err.Error(), tc.wantPhrase)
			}
		})
	}
}

func TestEmptyScenarioIsNoOp(t *testing.T) {
	sc := mustParse(t, "# only comments\n\n")
	if sc.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", sc.Len())
	}
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 1))
	before := c.PriceShift()
	n := sc.applyDue(c, time.Hour)
	if n != 0 {
		t.Fatalf("applyDue fired %d events on empty scenario", n)
	}
	if c.PriceShift() != before {
		t.Fatalf("empty scenario mutated controls")
	}
}

// TestApplyDueTimeline drives the runner core with a synthetic elapsed
// sequence (no real sleeps) and asserts the right control is set at the right
// threshold.
func TestApplyDueTimeline(t *testing.T) {
	jsonl := `{"at_ms": 0, "action": "price_shift", "params": {"offset_bps": 15, "scale": 1.0}}
{"at_ms": 5000, "action": "latency", "params": {"feed_to_book_ms": 50, "jitter_ms": 0}}
{"at_ms": 10000, "action": "price_shift", "params": {"offset_bps": 0}}
`
	sc := mustParse(t, jsonl)
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 42))

	// t < 0ms: nothing applied yet (use a negative-ish probe: elapsed just
	// before 0 is impossible, so probe at exactly 0 to fire only the t=0 event).
	// Before any apply, the seeded latency is zero.
	if got := c.Latency().FeedToBookDelay(); got != 0 {
		t.Fatalf("initial latency delay = %v, want 0", got)
	}

	// At t=0: the 15bps shift fires; latency untouched.
	sc.applyDue(c, 0)
	if got := c.PriceShift().OffsetBps; got != 15 {
		t.Fatalf("at 0ms, offset_bps = %v, want 15", got)
	}
	if got := c.Latency().FeedToBookDelay(); got != 0 {
		t.Fatalf("at 0ms, latency should be unchanged (0), got %v", got)
	}

	// At t=4999ms: still the t=0 shift, latency still zero (5000 not due).
	sc.applyDue(c, 4999*time.Millisecond)
	if got := c.PriceShift().OffsetBps; got != 15 {
		t.Fatalf("at 4999ms, offset_bps = %v, want 15", got)
	}
	if got := c.Latency().FeedToBookDelay(); got != 0 {
		t.Fatalf("at 4999ms, latency must still be 0, got %v", got)
	}

	// At t=5000ms: latency updated to 50ms; shift still 15bps.
	sc.applyDue(c, 5000*time.Millisecond)
	if got := c.Latency().FeedToBookDelay(); got != 50*time.Millisecond {
		t.Fatalf("at 5000ms, latency delay = %v, want 50ms", got)
	}
	if got := c.PriceShift().OffsetBps; got != 15 {
		t.Fatalf("at 5000ms, offset_bps = %v, want 15", got)
	}

	// At t=10000ms: shift reset to identity.
	sc.applyDue(c, 10000*time.Millisecond)
	if !c.PriceShift().IsIdentity() {
		t.Fatalf("at 10000ms, shift should be identity, got %+v", c.PriceShift())
	}

	// All events fired exactly once.
	if sc.fired != 3 {
		t.Fatalf("fired = %d, want 3", sc.fired)
	}
	// A further applyDue fires nothing.
	if n := sc.applyDue(c, time.Hour); n != 0 {
		t.Fatalf("re-apply fired %d events, want 0", n)
	}
}

// TestApplyDueShiftsSampleEvent is the end-to-end-ish check: the scheduled
// price shift, once applied via applyDue, actually shifts a sample feed event
// by the configured offset at the right time.
func TestApplyDueShiftsSampleEvent(t *testing.T) {
	jsonl := `{"at_ms": 0, "action": "price_shift", "params": {"offset_bps": 100, "scale": 1.0}}
{"at_ms": 10000, "action": "price_shift", "params": {"offset_bps": 0}}
`
	sc := mustParse(t, jsonl)
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 0))

	sample := feed.Event{
		Kind: feed.EventTrade,
		Trade: &feed.Trade{
			Instrument: "BTC-USD",
			Price:      100.0,
			Quantity:   1.0,
			Side:       "buy",
		},
	}

	// Before t=0, identity shift: price unchanged.
	out := c.PriceShift().ApplyEvent(sample)
	if out.Trade.Price != 100.0 {
		t.Fatalf("pre-scenario price = %v, want 100", out.Trade.Price)
	}

	// At t=0: 100bps = +1% → 101.
	sc.applyDue(c, 0)
	out = c.PriceShift().ApplyEvent(sample)
	if out.Trade.Price != 101.0 {
		t.Fatalf("at 0ms, shifted price = %v, want 101", out.Trade.Price)
	}

	// At t=10000ms: shift closed, price back to 100.
	sc.applyDue(c, 10000*time.Millisecond)
	out = c.PriceShift().ApplyEvent(sample)
	if out.Trade.Price != 100.0 {
		t.Fatalf("at 10000ms, price = %v, want 100 (dislocation closed)", out.Trade.Price)
	}
}

// TestRunFiresOnTimeline drives the real Run() with a high speed so the whole
// timeline fires in a few ms — verifies the wall-clock path wires applyDue
// correctly and respects completion.
func TestRunFiresOnTimeline(t *testing.T) {
	jsonl := `{"at_ms": 0, "action": "price_shift", "params": {"offset_bps": 20}}
{"at_ms": 1000, "action": "price_shift", "params": {"offset_bps": 0}}
`
	sc := mustParse(t, jsonl)
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// speed=1000 → 1000ms timeline runs in ~1ms.
	if err := sc.Run(ctx, c, time.Now(), 1000); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !c.PriceShift().IsIdentity() {
		t.Fatalf("after Run, shift should be identity (last event), got %+v", c.PriceShift())
	}
	if sc.fired != 2 {
		t.Fatalf("fired = %d, want 2", sc.fired)
	}
}

func TestRunRespectsCancellation(t *testing.T) {
	jsonl := `{"at_ms": 0, "action": "price_shift", "params": {"offset_bps": 5}}
{"at_ms": 60000, "action": "price_shift", "params": {"offset_bps": 0}}
`
	sc := mustParse(t, jsonl)
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 0))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := sc.Run(ctx, c, time.Now(), 1) // real time; second event is 60s out
	if err != context.Canceled {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	// The t=0 event still fired before we blocked on the 60s timer.
	if got := c.PriceShift().OffsetBps; got != 5 {
		t.Fatalf("t=0 event should have fired (offset 5), got %v", got)
	}
}
