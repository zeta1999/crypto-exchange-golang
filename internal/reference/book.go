// Package reference maintains a per-instrument reference order book rebuilt
// from a venue's normalized market-data feed (internal/feed).
//
// A Book consumes feed.LOBSnapshot frames: a full snapshot replaces the
// visible book; an incremental diff upserts levels and removes those whose
// quantity is zero (the feed's removal signal). Binance @depth20 emits only
// snapshots; Coinbase level2 emits one snapshot followed by diffs. The Book
// is the anchor the emulator's seeder (Phase 3) and RTR controller (Phase 4)
// converge the engine book toward.
package reference

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// Book is a single instrument's reference limit order book. It is safe for
// concurrent use: one writer (the feed consumer) calls Apply while many
// readers (emulator, API edges) call the read methods.
type Book struct {
	instrument string
	exchange   string

	mu   sync.Mutex
	bids map[string]feed.LOBLevel // key: price string; price -> level
	asks map[string]feed.LOBLevel

	initialized bool // a snapshot has been seen; diffs are applicable
	seqInit     bool // a non-zero sequence number has been observed
	lastSeq     uint64
	anomalies   uint64 // diffs dropped (no base, or out-of-order sequence)
	lastUpdate  time.Time

	// Cached sorted view, rebuilt lazily on read after a mutation so a burst
	// of diffs between two reads costs a single sort.
	dirty      bool
	sortedBids []feed.LOBLevel // descending price
	sortedAsks []feed.LOBLevel // ascending price
}

// NewBook returns an empty book for instrument on exchange.
func NewBook(instrument, exchange string) *Book {
	return &Book{
		instrument: instrument,
		exchange:   exchange,
		bids:       make(map[string]feed.LOBLevel),
		asks:       make(map[string]feed.LOBLevel),
	}
}

func (b *Book) Instrument() string { return b.instrument }
func (b *Book) Exchange() string   { return b.exchange }

// levelKey identifies a price level. The venue-committed decimal string is
// preferred (exact, avoids float-equality hazards); otherwise the float is
// formatted canonically. A single Book only ever mixes one venue's frames,
// so the keying scheme stays consistent within a book.
func levelKey(l feed.LOBLevel) string {
	if l.PriceDecimal != "" {
		return l.PriceDecimal
	}
	return strconv.FormatFloat(l.Price, 'f', -1, 64)
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
		b.bids = make(map[string]feed.LOBLevel, len(s.Bids))
		b.asks = make(map[string]feed.LOBLevel, len(s.Asks))
		for _, l := range s.Bids {
			if l.Quantity > 0 {
				b.bids[levelKey(l)] = l
			}
		}
		for _, l := range s.Asks {
			if l.Quantity > 0 {
				b.asks[levelKey(l)] = l
			}
		}
		b.initialized = true
		b.lastSeq = s.SequenceNumber
		b.seqInit = s.SequenceNumber > 0
		b.stamp(s.Timestamp)
		b.dirty = true
		return true
	}

	// Incremental diff.
	if !b.initialized {
		b.anomalies++ // can't apply a diff without a base snapshot
		return false
	}
	// Sequence numbers (when present, e.g. Coinbase) are strictly
	// increasing on a connection; a non-increasing value is a stale or
	// duplicate frame and must not be applied. Binance carries no sequence
	// (always 0), so the check is skipped there.
	if s.SequenceNumber > 0 {
		if b.seqInit && s.SequenceNumber <= b.lastSeq {
			b.anomalies++
			return false
		}
		b.lastSeq = s.SequenceNumber
		b.seqInit = true
	}
	applySide(b.bids, s.Bids)
	applySide(b.asks, s.Asks)
	b.stamp(s.Timestamp)
	b.dirty = true
	return true
}

// applySide upserts non-zero levels and removes zero-quantity ones.
func applySide(side map[string]feed.LOBLevel, levels []feed.LOBLevel) {
	for _, l := range levels {
		k := levelKey(l)
		if l.Quantity == 0 {
			delete(side, k)
		} else {
			side[k] = l
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

// rebuild refreshes the cached sorted view. Caller must hold b.mu.
func (b *Book) rebuild() {
	if !b.dirty {
		return
	}
	b.sortedBids = sortedLevels(b.bids, true)
	b.sortedAsks = sortedLevels(b.asks, false)
	b.dirty = false
}

func sortedLevels(m map[string]feed.LOBLevel, desc bool) []feed.LOBLevel {
	out := make([]feed.LOBLevel, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return out[i].Price > out[j].Price
		}
		return out[i].Price < out[j].Price
	})
	return out
}
