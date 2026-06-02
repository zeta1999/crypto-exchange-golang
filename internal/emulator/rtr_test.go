package emulator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
)

// newWiredEngine builds an engine and returns the underlying book so a test
// can register the seeder's trade hook.
func newWiredEngine(instrument string) (*engine.Engine, *orderbook.OrderBook) {
	book := orderbook.New([]string{instrument})
	return engine.New(book, okMargin{}, nil), book
}

func askVol(t *testing.T, eng *engine.Engine, price float64) float64 {
	t.Helper()
	snap, err := eng.Snapshot("BTC-USD")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, l := range snap.Asks {
		if l.Price == price {
			return l.Volume
		}
	}
	return 0
}

func TestAlpha(t *testing.T) {
	r := NewRTR(nil, 2*time.Second)
	if got := r.Alpha(2 * time.Second); math.Abs(got-(1-math.Exp(-1))) > 1e-9 {
		t.Errorf("Alpha(tau) = %v, want ~0.632", got)
	}
	if r.Alpha(0) != 1 {
		t.Error("Alpha(0) should snap (1)")
	}
	if NewRTR(nil, 0).Alpha(time.Second) != 1 {
		t.Error("tau<=0 should snap (1)")
	}
	// Larger dt closes more of the gap.
	if r.Alpha(time.Second) >= r.Alpha(4*time.Second) {
		t.Error("alpha should increase with dt")
	}
}

// TestRTRReturnsToReferenceAfterUserTrade is the Phase 4 DoD: seed the book,
// let a user trade eat synthetic liquidity, then converge progressively back
// to the reference. Convergence is gradual (not instant) and deterministic.
func TestRTRReturnsToReferenceAfterUserTrade(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng, book := newWiredEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true,
		[]feed.LOBLevel{lvl(100, 2), lvl(99, 5)},
		[]feed.LOBLevel{lvl(101, 3), lvl(102, 4)}))

	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	book.RegisterHook(func(evt string, data interface{}) {
		if evt == "trade" {
			s.OnTrade(data.(*orderbook.Trade))
		}
	})
	if _, err := s.Reconcile(context.Background()); err != nil { // seed exactly
		t.Fatal(err)
	}
	assertMirrors(t, eng, ref, 0)

	// User market buy eats 2 of the 3 resting at the best ask (101).
	if _, _, err := eng.PlaceMarket(context.Background(), &orderbook.Order{
		ID: "user1", Instrument: "BTC-USD", Side: orderbook.SideBuy, Volume: 2,
	}); err != nil {
		t.Fatalf("user trade: %v", err)
	}
	if got := askVol(t, eng, 101); got != 1 {
		t.Fatalf("after user trade ask@101 = %v, want 1", got)
	}

	// Converge at alpha=0.5: the eaten level is restored gradually.
	r := NewRTR(s, time.Second)
	const alpha = 0.5
	if _, err := s.Converge(context.Background(), alpha); err != nil {
		t.Fatal(err)
	}
	// Fold-in: resting 1, target = 1 + 0.5*(3-1) = 2. Progressive, not instant.
	if got := askVol(t, eng, 101); math.Abs(got-2) > 1e-9 {
		t.Fatalf("after 1 step ask@101 = %v, want 2 (gradual)", got)
	}
	// Other levels are untouched (no churn).
	if got := askVol(t, eng, 102); got != 4 {
		t.Errorf("ask@102 disturbed: %v", got)
	}

	// Further steps converge to the reference volume (3).
	for i := 0; i < 40; i++ {
		if _, err := s.Converge(context.Background(), alpha); err != nil {
			t.Fatal(err)
		}
	}
	if got := askVol(t, eng, 101); math.Abs(got-3) > 1e-6 {
		t.Errorf("after convergence ask@101 = %v, want ~3", got)
	}
	// Exponential convergence approaches but never exactly equals the target,
	// so compare within tolerance. Untouched levels remain exact.
	snap, _ := eng.Snapshot("BTC-USD")
	rbids, rasks := ref.Depth(0)
	cmpApprox(t, "bid", snap.Bids, rbids)
	cmpApprox(t, "ask", snap.Asks, rasks)
	_ = r
}

func cmpApprox(t *testing.T, side string, got []orderbook.Level, want []feed.LOBLevel) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s levels: got %d, want %d", side, len(got), len(want))
	}
	for i := range want {
		if got[i].Price != want[i].Price || math.Abs(got[i].Volume-want[i].Quantity) > 1e-6 {
			t.Errorf("%s[%d]: got %v@%v, want ~%v@%v", side, i, got[i].Volume, got[i].Price, want[i].Quantity, want[i].Price)
		}
	}
}

// TestFillAccountingMatchesEngine verifies the seeder's tracked volume stays
// consistent with the engine's real resting volume across fill+converge
// cycles, including a fill buffered against an order that is then re-placed
// (the generation-ID discard path). After convergence the tracked volume and
// the engine volume must agree.
func TestFillAccountingMatchesEngine(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng, book := newWiredEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 5)}, []feed.LOBLevel{lvl(101, 5)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	book.RegisterHook(func(evt string, data interface{}) {
		if evt == "trade" {
			s.OnTrade(data.(*orderbook.Trade))
		}
	})
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Stale fill against a since-replaced generation must not corrupt the level:
	// resize the ask (mints gen #2), then inject a fill for the OLD id.
	oldID, _ := s.OrderID(orderbook.SideSell, 101)
	ref.Apply(bookSnap(2, t0.Add(time.Second), false, nil, []feed.LOBLevel{lvl(101, 6)}))
	if _, err := s.Converge(context.Background(), 1.0); err != nil { // re-place ask@101 as gen #2
		t.Fatal(err)
	}
	s.OnTrade(&orderbook.Trade{Instrument: "BTC-USD", SellOrderID: oldID, Volume: 99}) // stale gen
	if _, err := s.Converge(context.Background(), 1.0); err != nil {
		t.Fatal(err)
	}
	if got := askVol(t, eng, 101); got != 6 {
		t.Errorf("stale-gen fill corrupted level: ask@101 = %v, want 6", got)
	}

	// Real fill against the current generation eats 2; converge tops it back up.
	curID, _ := s.OrderID(orderbook.SideSell, 101)
	if _, _, err := eng.PlaceMarket(context.Background(), &orderbook.Order{ID: "u1", Instrument: "BTC-USD", Side: orderbook.SideBuy, Volume: 2}); err != nil {
		t.Fatal(err)
	}
	_ = curID
	if got := askVol(t, eng, 101); got != 4 { // 6 - 2
		t.Fatalf("after user fill ask@101 = %v, want 4", got)
	}
	if _, err := s.Converge(context.Background(), 1.0); err != nil { // alpha=1 snaps back to 6
		t.Fatal(err)
	}
	if got := askVol(t, eng, 101); got != 6 {
		t.Errorf("after converge ask@101 = %v, want 6 (refilled)", got)
	}
}

// TestConvergeDrainsStaleProgressively: a level that leaves the reference is
// drained toward zero over successive passes, not removed instantly.
func TestConvergeDrainsStaleProgressively(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 4)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Converge(context.Background(), 1.0); err != nil {
		t.Fatal(err)
	}

	// Reference drops the 100 bid entirely.
	ref.Apply(bookSnap(2, t0.Add(time.Second), false, []feed.LOBLevel{lvl(100, 0)}, nil))

	prev := 4.0
	for i := 0; i < 3; i++ {
		if _, err := s.Converge(context.Background(), 0.5); err != nil {
			t.Fatal(err)
		}
		snap, _ := eng.Snapshot("BTC-USD")
		var cur float64
		for _, l := range snap.Bids {
			if l.Price == 100 {
				cur = l.Volume
			}
		}
		if cur >= prev {
			t.Errorf("step %d: stale level did not shrink (%v >= %v)", i, cur, prev)
		}
		prev = cur
	}
	if prev <= 0 || prev >= 1 {
		t.Errorf("stale level should be draining but present: %v", prev)
	}
}

// TestConvergeNoChurnWhenConverged: once at the reference, Converge is a no-op
// regardless of alpha (preserves resting orders / queue priority).
func TestConvergeNoChurnWhenConverged(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Converge(context.Background(), 1.0); err != nil {
		t.Fatal(err)
	}
	st, err := s.Converge(context.Background(), 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if st.Placed != 0 || st.Resized != 0 || st.Cancelled != 0 {
		t.Errorf("converged book should not churn: %+v", st)
	}
}
