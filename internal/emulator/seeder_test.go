package emulator

import (
	"context"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
)

// okMargin is a pass-through margin validator for tests.
type okMargin struct{}

func (okMargin) Validate(context.Context, *orderbook.Order) error { return nil }

func newEngine(instrument string) *engine.Engine {
	return engine.New(orderbook.New([]string{instrument}), okMargin{}, nil)
}

func lvl(p, q float64) feed.LOBLevel { return feed.LOBLevel{Price: p, Quantity: q} }

func bookSnap(seq uint64, ts time.Time, snapshot bool, bids, asks []feed.LOBLevel) *feed.LOBSnapshot {
	return &feed.LOBSnapshot{Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: ts, SequenceNumber: seq, Snapshot: snapshot, Bids: bids, Asks: asks}
}

// assertMirrors checks the engine book exactly reproduces the reference depth.
func assertMirrors(t *testing.T, eng *engine.Engine, ref *reference.Book, depth int) {
	t.Helper()
	snap, err := eng.Snapshot("BTC-USD")
	if err != nil {
		t.Fatalf("engine snapshot: %v", err)
	}
	rbids, rasks := ref.Depth(depth)
	cmp(t, "bid", snap.Bids, rbids)
	cmp(t, "ask", snap.Asks, rasks)
}

func cmp(t *testing.T, side string, got []orderbook.Level, want []feed.LOBLevel) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s levels: got %d, want %d (%+v vs %+v)", side, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Price != want[i].Price || got[i].Volume != want[i].Quantity {
			t.Errorf("%s[%d]: got %v@%v, want %v@%v", side, i, got[i].Volume, got[i].Price, want[i].Quantity, want[i].Price)
		}
	}
}

func TestSeedMirrorsReference(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2), lvl(99, 5)}, []feed.LOBLevel{lvl(101, 1), lvl(102, 3)}))

	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	st, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.Placed != 4 || st.Trades != 0 || st.Skipped {
		t.Fatalf("stats = %+v, want 4 placed / 0 trades / not skipped", st)
	}
	if s.SyntheticCount() != 4 {
		t.Errorf("synthetic count = %d, want 4", s.SyntheticCount())
	}
	assertMirrors(t, eng, ref, 0)
}

func TestReconcileIsIdempotent(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})

	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second pass over an unchanged reference must be a no-op.
	st, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Placed != 0 || st.Resized != 0 || st.Cancelled != 0 {
		t.Errorf("second reconcile not a no-op: %+v", st)
	}
}

func TestReconcileResizeAndCancel(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2), lvl(99, 5)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Diff: resize 100 -> 7, remove 99 (qty 0), add ask 102.
	ref.Apply(bookSnap(2, t0.Add(time.Second), false, []feed.LOBLevel{lvl(100, 7), lvl(99, 0)}, []feed.LOBLevel{lvl(102, 4)}))
	st, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Resized != 1 || st.Cancelled != 1 || st.Placed != 1 {
		t.Errorf("stats = %+v, want resized1/cancelled1/placed1", st)
	}
	assertMirrors(t, eng, ref, 0)
	if s.SyntheticCount() != 3 { // 100, 101, 102
		t.Errorf("synthetic count = %d, want 3", s.SyntheticCount())
	}
}

func TestReconcileSkipsUninitializedAndCrossed(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})

	// Uninitialized: skip, no orders.
	if st, _ := s.Reconcile(context.Background()); !st.Skipped {
		t.Error("expected skip on uninitialized reference")
	}
	if s.SyntheticCount() != 0 {
		t.Error("no synthetic orders should be placed")
	}

	// Crossed (bid 102 >= ask 101): skip.
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(102, 1)}, []feed.LOBLevel{lvl(101, 1)}))
	if !ref.Crossed() {
		t.Fatal("reference should be crossed")
	}
	if st, _ := s.Reconcile(context.Background()); !st.Skipped {
		t.Error("expected skip on crossed reference")
	}
	if s.SyntheticCount() != 0 {
		t.Error("crossed book should not be seeded")
	}
}

func TestDepthLevelsCap(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true,
		[]feed.LOBLevel{lvl(100, 1), lvl(99, 1), lvl(98, 1)},
		[]feed.LOBLevel{lvl(101, 1), lvl(102, 1), lvl(103, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD", DepthLevels: 2})
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.SyntheticCount() != 4 { // top 2 per side
		t.Errorf("synthetic count = %d, want 4 (2 per side)", s.SyntheticCount())
	}
	assertMirrors(t, eng, ref, 2)
}

func TestPriceShiftNeverSelfMatches(t *testing.T) {
	// When the whole book shifts up between passes, surviving and newly
	// placed synthetic orders are all current (uncrossed) reference levels,
	// so no placement may cross another synthetic order. Assert Trades==0
	// and the book mirrors after each step.
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})

	steps := []struct{ bids, asks []feed.LOBLevel }{
		{[]feed.LOBLevel{lvl(100, 2), lvl(99, 5)}, []feed.LOBLevel{lvl(101, 1), lvl(102, 3)}},
		// Shift up; touch moves past old levels, some keys persist on each side.
		{[]feed.LOBLevel{lvl(102, 2), lvl(101, 5)}, []feed.LOBLevel{lvl(103, 1), lvl(104, 3)}},
		// Narrow the spread; new bid sits just under the surviving ask.
		{[]feed.LOBLevel{lvl(102.5, 1), lvl(102, 2)}, []feed.LOBLevel{lvl(103, 1), lvl(104, 3)}},
	}
	for i, step := range steps {
		ref.Apply(bookSnap(uint64(i+1), t0.Add(time.Duration(i)*time.Second), true, step.bids, step.asks))
		st, err := s.Reconcile(context.Background())
		if err != nil {
			t.Fatalf("step %d reconcile: %v", i, err)
		}
		if st.Trades != 0 {
			t.Fatalf("step %d produced %d synthetic self-trades", i, st.Trades)
		}
		assertMirrors(t, eng, ref, 0)
	}
}

func TestReconcileToleratesExternallyRemovedOrder(t *testing.T) {
	// Simulate a synthetic order being filled/cancelled out-of-band (as
	// Phase 4/5 user flow will do): the engine no longer has it, but s.placed
	// still does. Reconcile must not abort on ErrOrderNotFound.
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2), lvl(99, 5)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Remove the 99 bid behind the seeder's back.
	if _, err := eng.CancelOrder(context.Background(), "BTC-USD", s.synthID(orderbook.SideBuy, 99)); err != nil {
		t.Fatalf("out-of-band cancel: %v", err)
	}

	// Reference drops 99 entirely; reconcile will try to cancel the already-
	// gone order. It must succeed, not abort.
	ref.Apply(bookSnap(2, t0.Add(time.Second), false, []feed.LOBLevel{lvl(99, 0)}, nil))
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile aborted on not-found cancel: %v", err)
	}
	assertMirrors(t, eng, ref, 0)

	// Clear must also tolerate an already-gone order.
	if _, err := eng.CancelOrder(context.Background(), "BTC-USD", s.synthID(orderbook.SideBuy, 100)); err != nil {
		t.Fatalf("out-of-band cancel: %v", err)
	}
	if err := s.Clear(context.Background()); err != nil {
		t.Fatalf("clear aborted on not-found cancel: %v", err)
	}
}

func TestClear(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	eng := newEngine("BTC-USD")
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, t0, true, []feed.LOBLevel{lvl(100, 2)}, []feed.LOBLevel{lvl(101, 1)}))
	s := NewSeeder(eng, ref, Config{Instrument: "BTC-USD"})
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(context.Background()); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if s.SyntheticCount() != 0 {
		t.Errorf("count after clear = %d, want 0", s.SyntheticCount())
	}
	snap, _ := eng.Snapshot("BTC-USD")
	if len(snap.Bids) != 0 || len(snap.Asks) != 0 {
		t.Errorf("engine book not empty after clear: %+v", snap)
	}
}
