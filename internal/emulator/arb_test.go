package emulator

import (
	"context"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func TestCrossArbDetector(t *testing.T) {
	// A: bid 100 / ask 101; B: bid 98 / ask 98.98 → buy-B-sell-A = 100-98.98 > 0.
	mkSnap := func(bid, ask float64) *orderbook.Snapshot {
		return &orderbook.Snapshot{
			Bids:    []orderbook.Level{{Price: decimal.FromFloat(bid), Volume: decimal.FromFloat(1)}},
			Asks:    []orderbook.Level{{Price: decimal.FromFloat(ask), Volume: decimal.FromFloat(1)}},
			BestBid: decimal.FromFloat(bid), BestAsk: decimal.FromFloat(ask),
		}
	}
	a := mkSnap(100, 101)
	b := mkSnap(98, 98.98)
	bba, basb := CrossArb(a, b)
	if bba.Sign() <= 0 {
		t.Errorf("buyBSellA = %v, want > 0", bba)
	}
	if basb.Sign() > 0 {
		t.Errorf("buyASellB = %v, want <= 0", basb)
	}
	// Aligned books → no arb either way.
	bba, basb = CrossArb(a, a)
	if bba.Sign() > 0 || basb.Sign() > 0 {
		t.Errorf("aligned books should have no arb: %v / %v", bba, basb)
	}
	// Empty side → no opportunity (no panic, no false arb).
	if x, y := CrossArb(&orderbook.Snapshot{}, b); x.Sign() > 0 || y.Sign() > 0 {
		t.Errorf("empty book A produced arb: %v / %v", x, y)
	}
}

// TestArbHarnessExploitableThenCloses drives two venues apart with a price
// shift, shows the cross-venue arbitrage is real (executable at a profit), then
// realigns the venues and shows the arb closes.
func TestArbHarnessExploitableThenCloses(t *testing.T) {
	ctx := context.Background()
	book := orderbook.New([]string{"BTC-A", "BTC-B"})
	eng := engine.New(book, okMargin{}, nil)

	// Venue A undislocated; venue B shifted down 2% (offset_bps -200).
	h := NewArbHarness(eng, "test", 0, "BTC-A", PriceShift{}, "BTC-B", NewPriceShift(-200, 0))
	snap := bookSnap(1, time.Unix(1700000000, 0), true,
		[]feed.LOBLevel{lvl(100, 5)}, []feed.LOBLevel{lvl(101, 5)})
	h.Feed(feed.Event{Kind: feed.EventBook, Book: snap})
	if err := h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	sa, _ := eng.Snapshot("BTC-A")
	sb, _ := eng.Snapshot("BTC-B")
	bba, basb := CrossArb(sa, sb)
	if bba.Sign() <= 0 {
		t.Fatalf("expected exploitable buy-B-sell-A arb; A %v/%v B %v/%v (spread %v)",
			sa.BestBid, sa.BestAsk, sb.BestBid, sb.BestAsk, bba)
	}
	if basb.Sign() > 0 {
		t.Errorf("reverse direction should not be an arb: %v", basb)
	}

	// Execute it: buy on the cheap venue (B) at its ask, sell on the rich venue
	// (A) at its bid — both legs must fill against the seeded liquidity.
	tb, _, err := eng.PlaceLimit(ctx, &orderbook.Order{
		ID: "arb-buy-b", Instrument: "BTC-B", Side: orderbook.SideBuy, Price: sb.BestAsk, Volume: decimal.FromFloat(1)})
	if err != nil || len(tb) == 0 {
		t.Fatalf("buy leg on B did not fill: trades=%d err=%v", len(tb), err)
	}
	ta, _, err := eng.PlaceLimit(ctx, &orderbook.Order{
		ID: "arb-sell-a", Instrument: "BTC-A", Side: orderbook.SideSell, Price: sa.BestBid, Volume: decimal.FromFloat(1)})
	if err != nil || len(ta) == 0 {
		t.Fatalf("sell leg on A did not fill: trades=%d err=%v", len(ta), err)
	}
	// Realized: bought at askB, sold at bidA → profit per unit > 0.
	bought, sold := tb[0].Price, ta[0].Price
	if sold.Sub(bought).Sign() <= 0 {
		t.Errorf("arb not profitable: bought B@%v, sold A@%v", bought, sold)
	}

	// Close the dislocation: realign venue B and re-mirror.
	if !h.SetShift("BTC-B", PriceShift{}) {
		t.Fatal("SetShift did not match the BTC-B leg")
	}
	if h.SetShift("nope", PriceShift{}) {
		t.Error("SetShift matched a nonexistent leg")
	}
	h.Feed(feed.Event{Kind: feed.EventBook, Book: snap})
	if err := h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sa, _ = eng.Snapshot("BTC-A")
	sb, _ = eng.Snapshot("BTC-B")
	if bba, _ := CrossArb(sa, sb); bba.Sign() > 0 {
		t.Errorf("arb should have closed after realign; A %v/%v B %v/%v (spread %v)",
			sa.BestBid, sa.BestAsk, sb.BestBid, sb.BestAsk, bba)
	}
}
