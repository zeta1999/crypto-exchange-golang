package coinbase

import (
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// level2Differ tracks the last broadcast book per product so the level2 channel
// can emit true incremental "update" frames (only the price levels that changed
// since the previous push, removals carried as new_quantity "0") rather than
// re-sending the whole top-N book each tick.
//
// Consistency model (no sequence gaps for a subscriber): the per-product
// baseline is the single source of truth. A new subscriber's "snapshot" frame is
// built from the CURRENT baseline (seeded lazily from a fresh engine snapshot),
// and every "update" diffs against — and then advances — that same baseline. A
// subscriber that joins during the diff/broadcast window may receive an update
// it already has in its snapshot, but level2 new_quantity values are ABSOLUTE,
// so re-applying a level is idempotent; a subscriber never MISSES a change.
type level2Differ struct {
	mu sync.Mutex
	st map[string]*l2Book // product -> last broadcast book
}

type l2Book struct {
	bids map[string]string // price -> quantity (StringPrec-formatted)
	asks map[string]string
}

func newLevel2Differ() *level2Differ {
	return &level2Differ{st: make(map[string]*l2Book)}
}

// levelMap renders order-book levels as a price->quantity map using the same
// formatting as the wire payload, so diffs compare like-for-like.
func levelMap(levels []orderbook.Level) map[string]string {
	m := make(map[string]string, len(levels))
	for _, lv := range levels {
		m[lv.Price.StringPrec(pricePrec)] = lv.Volume.StringPrec(sizePrec)
	}
	return m
}

// bookLocked returns the product's baseline, seeding it from snap if absent.
// Caller holds d.mu.
func (d *level2Differ) bookLocked(product string, snap *orderbook.Snapshot) *l2Book {
	b := d.st[product]
	if b == nil {
		b = &l2Book{bids: levelMap(snap.Bids), asks: levelMap(snap.Asks)}
		d.st[product] = b
	}
	return b
}

// snapshotUpdatesLocked returns the full current baseline as l2 updates (the
// "snapshot" frame content), seeding the baseline from snap if needed. Caller
// holds d.mu so the snapshot and the subscriber registration that follows are
// ordered against the diff ticker.
func (d *level2Differ) snapshotUpdatesLocked(product string, snap *orderbook.Snapshot, eventTime string) []l2Update {
	b := d.bookLocked(product, snap)
	out := make([]l2Update, 0, len(b.bids)+len(b.asks))
	out = mapUpdates(out, b.bids, "bid", eventTime)
	out = mapUpdates(out, b.asks, "offer", eventTime)
	return out
}

// diff locks the differ and computes the changed levels for product since the
// last push (used by tests). The ticker path uses diffLocked to hold the lock
// across the subsequent broadcast.
func (d *level2Differ) diff(product string, snap *orderbook.Snapshot, eventTime string) ([]l2Update, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.diffLocked(product, snap, eventTime)
}

// diffLocked computes the changed levels for product since the last push and
// advances the baseline. It seeds (and emits nothing) on the first sighting.
// Returns the changed levels and whether anything changed. Caller holds d.mu.
func (d *level2Differ) diffLocked(product string, snap *orderbook.Snapshot, eventTime string) ([]l2Update, bool) {
	curBids := levelMap(snap.Bids)
	curAsks := levelMap(snap.Asks)

	b := d.st[product]
	if b == nil { // first sighting: seed baseline, emit nothing.
		d.st[product] = &l2Book{bids: curBids, asks: curAsks}
		return nil, false
	}

	var updates []l2Update
	updates = diffSideL2(updates, b.bids, curBids, "bid", eventTime)
	updates = diffSideL2(updates, b.asks, curAsks, "offer", eventTime)
	if len(updates) == 0 {
		return nil, false
	}
	b.bids, b.asks = curBids, curAsks
	return updates, true
}

// mapUpdates appends every level in m as an l2 update of the given side.
func mapUpdates(dst []l2Update, m map[string]string, side, eventTime string) []l2Update {
	for price, qty := range m {
		dst = append(dst, l2Update{Side: side, EventTime: eventTime, PriceLevel: price, NewQuantity: qty})
	}
	return dst
}

// diffSideL2 appends the changed levels of one side: a new/changed quantity, or
// "0" for a level present before but gone now (Coinbase's removal convention).
func diffSideL2(dst []l2Update, prev, cur map[string]string, side, eventTime string) []l2Update {
	for price, qty := range cur {
		if prev[price] != qty {
			dst = append(dst, l2Update{Side: side, EventTime: eventTime, PriceLevel: price, NewQuantity: qty})
		}
	}
	for price := range prev {
		if _, ok := cur[price]; !ok {
			dst = append(dst, l2Update{Side: side, EventTime: eventTime, PriceLevel: price, NewQuantity: "0"})
		}
	}
	return dst
}
