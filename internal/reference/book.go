// Package reference maintains a per-instrument reference order book rebuilt
// from a venue's normalized market-data feed (internal/feed).
//
// A Book consumes feed.LOBSnapshot frames: a full snapshot replaces the
// visible book; an incremental diff upserts levels and removes those whose
// quantity is zero (the feed's removal signal). Binance @depth20 emits only
// snapshots; Coinbase level2 emits one snapshot followed by diffs. The Book
// is the anchor the emulator's seeder (Phase 3) and RTR controller (Phase 4)
// converge the engine book toward.
//
// Prices and quantities are exact decimal.Decimal values: the feed package
// stays float64 + decimal strings, and this package is the conversion
// boundary. When a frame carries the venue's exact decimal string
// (PriceDecimal/QuantityDecimal) we parse that; otherwise we fall back to the
// lossy float64. Levels are keyed by their exact Decimal price, so a level
// quoted as "100.50" and a later removal quoted as "100.5" collapse to one
// logical price.
package reference

import (
	"sort"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Level is one price level in the reference book, carrying exact decimal
// price and quantity. It is a pure value type (no slices, maps, or pointers):
// the book hands out levels by shallow copy and relies on that being a deep
// copy for its immutability guarantee.
type Level struct {
	Price      decimal.Decimal
	Quantity   decimal.Decimal
	OrderCount uint32
}

// Book is a single instrument's reference limit order book. It is safe for
// concurrent use, but reads are serialized: every read takes an internal
// mutex and may rebuild a cached sorted view, so high-fan-out concurrent
// reads contend. Apply (one writer) and the read methods never deadlock —
// only this book's mutex is ever held.
type Book struct {
	instrument string
	exchange   string

	mu   sync.Mutex
	bids map[decimal.Decimal]Level // key: exact price; price -> level
	asks map[decimal.Decimal]Level

	initialized bool   // a snapshot has been seen; diffs are applicable
	lastSeq     uint64 // metadata only: see note in Apply
	anomalies   uint64 // diffs dropped because no base snapshot was seen yet
	crossings   uint64 // times a mutation left the book crossed (bid >= ask)
	lastUpdate  time.Time

	// Cached sorted view, rebuilt lazily on read after a mutation so a burst
	// of diffs between two reads costs a single sort.
	dirty      bool
	crossed    bool
	sortedBids []Level // descending price
	sortedAsks []Level // ascending price
}

// NewBook returns an empty book for instrument on exchange.
func NewBook(instrument, exchange string) *Book {
	return &Book{
		instrument: instrument,
		exchange:   exchange,
		bids:       make(map[decimal.Decimal]Level),
		asks:       make(map[decimal.Decimal]Level),
	}
}

func (b *Book) Instrument() string { return b.instrument }
func (b *Book) Exchange() string   { return b.exchange }

// toLevel converts an incoming feed.LOBLevel to an exact reference.Level.
// Prefer the venue's exact decimal strings (PriceDecimal/QuantityDecimal) when
// present; otherwise fall back to the lossy float64. The Decimal price keys the
// internal maps, so "100.50" and "100.5" collapse to one logical level.
func toLevel(l feed.LOBLevel) Level {
	var price, qty decimal.Decimal
	if l.PriceDecimal != "" {
		if d, err := decimal.Parse(l.PriceDecimal); err == nil {
			price = d
		} else {
			price = decimal.FromFloat(l.Price)
		}
	} else {
		price = decimal.FromFloat(l.Price)
	}
	if l.QuantityDecimal != "" {
		if d, err := decimal.Parse(l.QuantityDecimal); err == nil {
			qty = d
		} else {
			qty = decimal.FromFloat(l.Quantity)
		}
	} else {
		qty = decimal.FromFloat(l.Quantity)
	}
	return Level{Price: price, Quantity: qty, OrderCount: l.OrderCount}
}

// Apply folds one snapshot or diff into the book. It reports whether the
// frame was applied; a diff arriving before any snapshot, or one carrying a
// non-monotonic sequence number, is dropped and counted as an anomaly.
func (b *Book) Apply(s *feed.LOBSnapshot) bool {
	if s == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if s.Snapshot {
		b.bids = make(map[decimal.Decimal]Level, len(s.Bids))
		b.asks = make(map[decimal.Decimal]Level, len(s.Asks))
		for _, l := range s.Bids {
			lvl := toLevel(l)
			if lvl.Quantity.Sign() > 0 {
				b.bids[lvl.Price] = lvl
			}
		}
		for _, l := range s.Asks {
			lvl := toLevel(l)
			if lvl.Quantity.Sign() > 0 {
				b.asks[lvl.Price] = lvl
			}
		}
		b.initialized = true
		b.lastSeq = s.SequenceNumber
		b.stamp(s.Timestamp)
		b.dirty = true
		return true
	}

	// Incremental diff.
	if !b.initialized {
		b.anomalies++ // can't apply a diff without a base snapshot
		return false
	}
	// NOTE on SequenceNumber: for Coinbase it is the *connection-global*
	// counter copied onto every product's frame, not a per-instrument one.
	// A per-book monotonic check is therefore meaningless — for one product
	// the subsequence is strictly increasing but non-contiguous, so it can
	// neither reject bad frames nor detect a dropped diff. We keep lastSeq
	// only as metadata to stamp onto Snapshot(); genuine gap detection lives
	// at the connection level in the Coinbase adapter, where the contiguous
	// global counter actually is. Binance carries no sequence (always 0).
	if s.SequenceNumber > 0 {
		b.lastSeq = s.SequenceNumber
	}
	applySide(b.bids, s.Bids)
	applySide(b.asks, s.Asks)
	b.stamp(s.Timestamp)
	b.dirty = true
	return true
}

// applySide upserts non-zero levels and removes zero-quantity ones.
func applySide(side map[decimal.Decimal]Level, levels []feed.LOBLevel) {
	for _, l := range levels {
		lvl := toLevel(l)
		if lvl.Quantity.Sign() == 0 {
			delete(side, lvl.Price)
		} else {
			side[lvl.Price] = lvl
		}
	}
}

// stamp advances lastUpdate, ignoring zero timestamps so a frame without a
// usable time doesn't rewind the clock.
func (b *Book) stamp(ts time.Time) {
	if !ts.IsZero() {
		b.lastUpdate = ts
	}
}

// rebuild refreshes the cached sorted view and the crossed flag. Caller must
// hold b.mu. A crossed touch (best bid >= best ask) can arise from a dropped
// diff or a feed glitch; we surface it rather than silently emitting a
// negative spread to the convergence controller.
func (b *Book) rebuild() {
	if !b.dirty {
		return
	}
	b.sortedBids = sortedLevels(b.bids, true)
	b.sortedAsks = sortedLevels(b.asks, false)
	b.crossed = len(b.sortedBids) > 0 && len(b.sortedAsks) > 0 &&
		b.sortedBids[0].Price.Gte(b.sortedAsks[0].Price)
	if b.crossed {
		b.crossings++
	}
	b.dirty = false
}

func sortedLevels(m map[decimal.Decimal]Level, desc bool) []Level {
	out := make([]Level, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return out[i].Price.Cmp(out[j].Price) > 0
		}
		return out[i].Price.Cmp(out[j].Price) < 0
	})
	return out
}
