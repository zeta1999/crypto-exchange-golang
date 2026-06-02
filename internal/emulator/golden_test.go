package emulator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/replay"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// detClock returns a deterministic monotonic clock (base + 1ms per call) so a
// run's trade timestamps are reproducible — unlike time.Now.
func detClock() func() time.Time {
	base := time.Unix(1_700_000_000, 0).UTC()
	var n int64
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// runDeterministic executes a fixed pipeline (recorded trace → reference →
// seeder → a crossing user order → a tape print) against an engine whose book
// uses a deterministic clock, and returns a full serialization of every
// resulting trade (incl. timestamp) plus the final book. Two calls must produce
// byte-identical output.
func runDeterministic() string {
	ctx := context.Background()
	book := orderbook.New([]string{"BTC-USD"})
	book.SetClock(detClock())
	eng := engine.New(book, okMargin{}, nil)

	var sb strings.Builder
	book.RegisterHook(func(evt string, data interface{}) {
		if evt != "trade" {
			return
		}
		t := data.(*orderbook.Trade)
		fmt.Fprintf(&sb, "TRADE %s %s@%s buy=%s sell=%s t=%d\n",
			t.TakerSide, t.Volume.String(), t.Price.String(),
			t.BuyOrderID, t.SellOrderID, t.ExecutedAt.UnixNano())
	})

	src := replay.New("../../testdata/feed/sample.jsonl")
	ch, _ := src.Start(ctx)
	refs := reference.NewSet()
	refs.Consume(ctx, ch)
	ref, _ := refs.Get("BTC-USD")

	seeder := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	_, _ = seeder.Reconcile(ctx)

	// A user buy that crosses the synthetic ask at 42001 → a deterministic fill.
	_, _, _ = eng.PlaceLimit(ctx, &orderbook.Order{
		ID: "user1", Instrument: "BTC-USD", Side: orderbook.SideBuy,
		Price: decimal.FromFloat(42001), Volume: decimal.FromFloat(0.3),
	})
	// A tape print sweeping a bit deeper.
	tape := NewTapeReplay(eng, "BTC-USD")
	_, _ = tape.Inject(ctx, tapeTrade("buy", 42002, 0.2))

	snap, _ := eng.Snapshot("BTC-USD")
	fmt.Fprintf(&sb, "BOOK bid=%s ask=%s bids=%d asks=%d\n",
		snap.BestBid.String(), snap.BestAsk.String(), len(snap.Bids), len(snap.Asks))
	return sb.String()
}

func TestDeterministicReplayIsReproducible(t *testing.T) {
	a := runDeterministic()
	b := runDeterministic()
	if a != b {
		t.Errorf("run not reproducible:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", a, b)
	}
	// The pipeline must actually have matched (otherwise the test is vacuous),
	// and reproducibility implies the deterministic clock drove the timestamps
	// (time.Now would differ between the two runs).
	if !strings.Contains(a, "TRADE ") {
		t.Errorf("expected at least one trade, got:\n%s", a)
	}
	// Sanity: the first trade timestamp is a deterministic clock value, not a
	// 2026 wall-clock nanos.
	if !strings.Contains(a, "t=1700000") {
		t.Errorf("trade timestamps not from the deterministic clock:\n%s", a)
	}
}
