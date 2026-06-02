package emulator

import (
	"context"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed/replay"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
)

// TestReplayPipelineSeedsEngine drives the whole offline path — a recorded
// JSONL trace (Phase 1 replay.Source) → reference.Set → Seeder → engine — and
// asserts the engine book deterministically mirrors the trace's final book.
// This is the "venue: replay" mode: the emulator runs reproducibly with no
// network. The committed fixture snapshots a BTC-USD book then removes its top
// bid via a diff.
func TestReplayPipelineSeedsEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	src := replay.New("../../testdata/feed/sample.jsonl")
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("replay start: %v", err)
	}
	refs := reference.NewSet()
	refs.Consume(ctx, ch) // finite source — returns when the trace is exhausted

	insts := refs.Instruments()
	if len(insts) != 1 || insts[0] != "BTC-USD" {
		t.Fatalf("instruments = %v, want [BTC-USD]", insts)
	}
	ref, _ := refs.Get("BTC-USD")

	eng := newEngine("BTC-USD")
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Engine book must equal the reference rebuilt from the trace.
	assertMirrors(t, eng, ref, 0)

	// Sanity: the trace's diff removed the 41999 bid, leaving 41998.5 best bid.
	bid, ask, ok := ref.BestBidAsk()
	if !ok || bid.Price.Float64() != 41998.5 || ask.Price.Float64() != 42001 {
		t.Errorf("trace-derived touch = %v/%v ok=%v", bid.Price, ask.Price, ok)
	}
}
