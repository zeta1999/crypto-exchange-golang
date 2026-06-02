package reference

import (
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func dec(s string) decimal.Decimal { return decimal.MustParse(s) }

func lvl(price, qty float64) feed.LOBLevel {
	return feed.LOBLevel{Price: price, Quantity: qty}
}

func snap(seq uint64, ts time.Time, bids, asks []feed.LOBLevel) *feed.LOBSnapshot {
	return &feed.LOBSnapshot{Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: ts, SequenceNumber: seq, Snapshot: true, Bids: bids, Asks: asks}
}

func diff(seq uint64, ts time.Time, bids, asks []feed.LOBLevel) *feed.LOBSnapshot {
	return &feed.LOBSnapshot{Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: ts, SequenceNumber: seq, Snapshot: false, Bids: bids, Asks: asks}
}

func TestSnapshotThenDiffStepState(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")

	// Step 1: snapshot.
	if !b.Apply(snap(1, t0, []feed.LOBLevel{lvl(100, 2), lvl(99, 5)}, []feed.LOBLevel{lvl(101, 1), lvl(102, 3)})) {
		t.Fatal("snapshot not applied")
	}
	bid, ask, ok := b.BestBidAsk()
	if !ok || !bid.Price.Eq(dec("100")) || !ask.Price.Eq(dec("101")) {
		t.Fatalf("after snap: bid=%v ask=%v ok=%v", bid.Price, ask.Price, ok)
	}
	if mid, _ := b.Mid(); !mid.Eq(dec("100.5")) {
		t.Errorf("mid = %v, want 100.5", mid)
	}
	if sp, _ := b.Spread(); !sp.Eq(dec("1")) {
		t.Errorf("spread = %v, want 1", sp)
	}

	// Step 2: diff improves the bid and adds an ask level.
	b.Apply(diff(2, t0.Add(time.Second), []feed.LOBLevel{lvl(100.5, 4)}, []feed.LOBLevel{lvl(100.75, 2)}))
	bid, ask, _ = b.BestBidAsk()
	if !bid.Price.Eq(dec("100.5")) || !ask.Price.Eq(dec("100.75")) {
		t.Errorf("after diff1: bid=%v ask=%v", bid.Price, ask.Price)
	}

	// Step 3: diff removes the top bid (qty 0) -> best bid falls back to 100.
	b.Apply(diff(3, t0.Add(2*time.Second), []feed.LOBLevel{lvl(100.5, 0)}, nil))
	bid, _, _ = b.BestBidAsk()
	if !bid.Price.Eq(dec("100")) {
		t.Errorf("after removal: best bid = %v, want 100", bid.Price)
	}

	// Depth reflects remaining levels, sorted.
	bids, asks := b.Depth(10)
	if len(bids) != 2 || !bids[0].Price.Eq(dec("100")) || !bids[1].Price.Eq(dec("99")) {
		t.Errorf("bids = %+v", bids)
	}
	if len(asks) != 3 || !asks[0].Price.Eq(dec("100.75")) {
		t.Errorf("asks = %+v", asks)
	}
	if got := b.LastUpdate(); !got.Equal(t0.Add(2 * time.Second)) {
		t.Errorf("lastUpdate = %v", got)
	}
}

func TestDiffBeforeSnapshotIsAnomaly(t *testing.T) {
	b := NewBook("BTC-USD", "coinbase")
	if b.Apply(diff(1, time.Now(), []feed.LOBLevel{lvl(100, 1)}, nil)) {
		t.Error("diff before snapshot should not apply")
	}
	if b.Initialized() {
		t.Error("book should not be initialized by a diff")
	}
	if b.Anomalies() != 1 {
		t.Errorf("anomalies = %d, want 1", b.Anomalies())
	}
}

func TestSequenceIsMetadataNotEnforced(t *testing.T) {
	// SequenceNumber is connection-global (Coinbase), so the per-book code
	// must NOT reject frames on it — gap detection lives in the adapter.
	// A diff carrying a "lower" seq still applies; lastSeq is just metadata
	// surfaced on Snapshot().
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	b.Apply(snap(10, t0, []feed.LOBLevel{lvl(100, 1)}, []feed.LOBLevel{lvl(101, 1)}))
	if !b.Apply(diff(9, t0, []feed.LOBLevel{lvl(100, 7)}, nil)) {
		t.Error("diff must apply regardless of connection-global seq ordering")
	}
	bid, _ := b.BestBid()
	if !bid.Quantity.Eq(dec("7")) {
		t.Errorf("diff not applied: qty=%v, want 7", bid.Quantity)
	}
	if b.Anomalies() != 0 {
		t.Errorf("anomalies = %d, want 0 (no per-book seq enforcement)", b.Anomalies())
	}
	if b.Snapshot().SequenceNumber != 9 {
		t.Errorf("snapshot seq = %d, want 9 (latest as metadata)", b.Snapshot().SequenceNumber)
	}
}

func TestCrossedBookDetected(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	b.Apply(snap(1, t0, []feed.LOBLevel{lvl(100, 1)}, []feed.LOBLevel{lvl(101, 1)}))
	if b.Crossed() {
		t.Fatal("normal book should not be crossed")
	}
	// A diff lifts the bid above the ask -> crossed.
	b.Apply(diff(2, t0, []feed.LOBLevel{lvl(102, 1)}, nil))
	if !b.Crossed() {
		t.Error("book with bid 102 >= ask 101 should be crossed")
	}
	if b.Crossings() == 0 {
		t.Error("crossing should be counted")
	}
	if sp, _ := b.Spread(); sp.Sign() >= 0 {
		t.Errorf("crossed spread should be negative, got %v", sp)
	}
}

func TestKeyingCollapsesDecimalAndFloat(t *testing.T) {
	// A level keyed via PriceDecimal "100.50" and a later removal carrying
	// only the float 100.5 must hit the same key (no phantom level).
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	b.Apply(&feed.LOBSnapshot{Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: t0, Snapshot: true,
		Bids: []feed.LOBLevel{{Price: 100.5, Quantity: 2, PriceDecimal: "100.50"}},
		Asks: []feed.LOBLevel{lvl(101, 1)}})
	// Diff removes 100.5 using the float-only form.
	b.Apply(diff(2, t0, []feed.LOBLevel{lvl(100.5, 0)}, nil))
	if bids, _ := b.Depth(0); len(bids) != 0 {
		t.Errorf("decimal/float key mismatch left a phantom level: %+v", bids)
	}
}

func TestBinanceStyleSnapshotsNoSequence(t *testing.T) {
	// Binance carries seq 0 on every (full) snapshot; successive snapshots
	// must each replace the book without tripping the sequence check.
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTCUSDT", "binance")
	b.Apply(&feed.LOBSnapshot{Instrument: "BTCUSDT", Exchange: "binance", Timestamp: t0, Snapshot: true, Bids: []feed.LOBLevel{lvl(100, 1)}, Asks: []feed.LOBLevel{lvl(101, 1)}})
	b.Apply(&feed.LOBSnapshot{Instrument: "BTCUSDT", Exchange: "binance", Timestamp: t0.Add(time.Second), Snapshot: true, Bids: []feed.LOBLevel{lvl(105, 2)}, Asks: []feed.LOBLevel{lvl(106, 2)}})
	bid, ask, ok := b.BestBidAsk()
	if !ok || !bid.Price.Eq(dec("105")) || !ask.Price.Eq(dec("106")) {
		t.Errorf("second snapshot did not replace book: bid=%v ask=%v", bid.Price, ask.Price)
	}
	if b.Anomalies() != 0 {
		t.Errorf("anomalies = %d, want 0", b.Anomalies())
	}
}

func TestSnapshotIsImmutable(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	b.Apply(snap(1, t0, []feed.LOBLevel{lvl(100, 2)}, []feed.LOBLevel{lvl(101, 1)}))

	s := b.Snapshot()
	s.Bids[0].Quantity = dec("999") // mutate the returned copy

	bid, _ := b.BestBid()
	if !bid.Quantity.Eq(dec("2")) {
		t.Errorf("internal book mutated via returned snapshot: qty=%v", bid.Quantity)
	}
}

func TestEmptySideReads(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	b.Apply(snap(1, t0, []feed.LOBLevel{lvl(100, 1)}, nil)) // no asks
	if _, _, ok := b.BestBidAsk(); ok {
		t.Error("BestBidAsk should be !ok with empty ask side")
	}
	if _, ok := b.Mid(); ok {
		t.Error("Mid should be !ok with empty ask side")
	}
	if _, ok := b.BestBid(); !ok {
		t.Error("BestBid should be ok")
	}
}

func TestStale(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	b := NewBook("BTC-USD", "coinbase")
	if !b.Stale(t0, time.Minute) {
		t.Error("uninitialized book should be stale")
	}
	b.Apply(snap(1, t0, []feed.LOBLevel{lvl(100, 1)}, []feed.LOBLevel{lvl(101, 1)}))
	if b.Stale(t0.Add(30*time.Second), time.Minute) {
		t.Error("fresh book should not be stale within maxAge")
	}
	if !b.Stale(t0.Add(2*time.Minute), time.Minute) {
		t.Error("book older than maxAge should be stale")
	}
	if b.Stale(t0.Add(time.Hour), 0) {
		t.Error("maxAge<=0 disables the age check")
	}
}
