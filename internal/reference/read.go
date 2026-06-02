package reference

import (
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Snapshot is an immutable copy of a reference book's visible state, with
// exact decimal levels. It mirrors the shape of feed.LOBSnapshot but uses
// reference.Level (Decimal) so downstream consumers price off exact values.
type Snapshot struct {
	Instrument     string
	Exchange       string
	Timestamp      time.Time
	SequenceNumber uint64
	Bids           []Level // descending price
	Asks           []Level // ascending price
}

// Initialized reports whether at least one snapshot has been applied.
func (b *Book) Initialized() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.initialized
}

// Anomalies returns the count of dropped frames (diffs without a base, or
// out-of-order sequence numbers) — a health signal for the feed.
func (b *Book) Anomalies() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.anomalies
}

// LastUpdate returns the venue-provided timestamp of the most recently
// applied frame. Note this is event/feed time, not wall-clock receipt time;
// Stale is measured against it.
func (b *Book) LastUpdate() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUpdate
}

// Crossed reports whether the current touch is crossed (best bid >= best
// ask), which indicates a degraded/glitched book. Consumers that price off
// Mid/Spread should check this first.
func (b *Book) Crossed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	return b.crossed
}

// Crossings returns how many times a mutation has left the book crossed — a
// health signal for the feed.
func (b *Book) Crossings() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	return b.crossings
}

// Stale reports whether the book has not updated within maxAge of now, or
// has never been initialized. A non-positive maxAge disables the age check
// (only the initialization state matters).
func (b *Book) Stale(now time.Time, maxAge time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.initialized {
		return true
	}
	if maxAge <= 0 {
		return false
	}
	return now.Sub(b.lastUpdate) > maxAge
}

// BestBid returns the highest bid level, or ok=false if the bid side is empty.
func (b *Book) BestBid() (Level, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	if len(b.sortedBids) == 0 {
		return Level{}, false
	}
	return b.sortedBids[0], true
}

// BestAsk returns the lowest ask level, or ok=false if the ask side is empty.
func (b *Book) BestAsk() (Level, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	if len(b.sortedAsks) == 0 {
		return Level{}, false
	}
	return b.sortedAsks[0], true
}

// BestBidAsk returns both tops; ok is false unless both sides are non-empty.
func (b *Book) BestBidAsk() (bid, ask Level, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	if len(b.sortedBids) == 0 || len(b.sortedAsks) == 0 {
		return Level{}, Level{}, false
	}
	return b.sortedBids[0], b.sortedAsks[0], true
}

// Mid returns the exact midpoint of the touch, or ok=false if either side is
// empty.
func (b *Book) Mid() (decimal.Decimal, bool) {
	bid, ask, ok := b.BestBidAsk()
	if !ok {
		return decimal.Zero, false
	}
	return bid.Price.Add(ask.Price).Div(decimal.FromInt(2)), true
}

// Spread returns best ask minus best bid, or ok=false if either side is
// empty. It can be negative if the book is crossed (see Crossed).
func (b *Book) Spread() (decimal.Decimal, bool) {
	bid, ask, ok := b.BestBidAsk()
	if !ok {
		return decimal.Zero, false
	}
	return ask.Price.Sub(bid.Price), true
}

// Depth returns copies of the top n levels per side (n <= 0 means all),
// sorted bids descending and asks ascending. The returned slices are owned
// by the caller and safe to mutate.
func (b *Book) Depth(n int) (bids, asks []Level) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	return clip(b.sortedBids, n), clip(b.sortedAsks, n)
}

func clip(s []Level, n int) []Level {
	if n <= 0 || n > len(s) {
		n = len(s)
	}
	out := make([]Level, n)
	copy(out, s[:n])
	return out
}

// Snapshot returns an immutable copy of the current book as a reference
// Snapshot, carrying the last sequence number and update time. Safe to hand
// to downstream consumers without exposing internal state.
func (b *Book) Snapshot() *Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rebuild()
	return &Snapshot{
		Instrument:     b.instrument,
		Exchange:       b.exchange,
		Timestamp:      b.lastUpdate,
		SequenceNumber: b.lastSeq,
		Bids:           clip(b.sortedBids, 0),
		Asks:           clip(b.sortedAsks, 0),
	}
}
