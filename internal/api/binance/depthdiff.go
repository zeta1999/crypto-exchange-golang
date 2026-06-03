package binance

import (
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// depthDiffer tracks the last broadcast book per engine instrument so the
// diff-depth stream (<sym>@depth) can emit only the levels that changed since
// the previous push, with Binance's monotonic U/u update-id semantics.
//
// The same per-symbol update id drives the REST GET /api/v3/depth
// `lastUpdateId`, so a client can take a REST snapshot and then apply buffered
// diffs (dropping any event whose final id `u` is <= the snapshot id) exactly
// as it would against the real venue.
//
// Diffs cover the FULL book (every changed level), like the real venue — so a
// client should sync against a full REST snapshot (GET /api/v3/depth without a
// small `limit`), not a truncated one, or it will see changes to levels outside
// its window.
//
// Update ids lazily initialise to now().UnixMilli() (a large, non-zero value
// like the real venue) and increment by one on every changed diff. The first
// diff() for a symbol silently seeds the baseline (the REST snapshot is the
// client's starting book), so a fresh subscriber never receives the whole book
// disguised as a single update.
type depthDiffer struct {
	mu  sync.Mutex
	now func() time.Time
	st  map[string]*symbolDepth // engine instrument -> last broadcast state
}

type symbolDepth struct {
	lastID int64
	bids   map[string]string // price -> qty (StringPrec-formatted, matching levelsToPairs)
	asks   map[string]string
}

func newDepthDiffer(now func() time.Time) *depthDiffer {
	if now == nil {
		now = time.Now
	}
	return &depthDiffer{now: now, st: make(map[string]*symbolDepth)}
}

// stateLocked returns (creating if needed) the per-symbol state. Caller holds mu.
func (d *depthDiffer) stateLocked(engineSym string) *symbolDepth {
	s := d.st[engineSym]
	if s == nil {
		s = &symbolDepth{lastID: d.now().UnixMilli()}
		d.st[engineSym] = s
	}
	return s
}

// current returns the symbol's current update id (for the REST snapshot's
// lastUpdateId) without advancing it.
func (d *depthDiffer) current(engineSym string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stateLocked(engineSym).lastID
}

// snapMaps builds price->qty maps from a snapshot using the same string
// formatting as the wire payload, so diffing compares like-for-like.
func snapMaps(snap *orderbook.Snapshot) (bids, asks map[string]string) {
	bids = make(map[string]string, len(snap.Bids))
	for _, lv := range snap.Bids {
		bids[lv.Price.StringPrec(pricePrec)] = lv.Volume.StringPrec(qtyPrec)
	}
	asks = make(map[string]string, len(snap.Asks))
	for _, lv := range snap.Asks {
		asks[lv.Price.StringPrec(pricePrec)] = lv.Volume.StringPrec(qtyPrec)
	}
	return bids, asks
}

// diffSide compares the previous and current level maps for one side, returning
// the changed levels as [price, qty] pairs. A price present before but gone now
// is emitted with qty "0" (Binance's "level removed" convention).
func diffSide(prev, cur map[string]string) [][2]string {
	var out [][2]string
	for price, qty := range cur {
		if prev[price] != qty {
			out = append(out, [2]string{price, qty})
		}
	}
	for price := range prev {
		if _, ok := cur[price]; !ok {
			out = append(out, [2]string{price, "0"})
		}
	}
	return out
}

// diff computes the changed levels since the last push for engineSym. On the
// first call for a symbol it silently seeds the baseline and reports
// changed=false. On a later call it advances the update id by one only when the
// book actually changed; firstID/finalID are the inclusive U/u range (here U==u
// since each push is a single update).
func (d *depthDiffer) diff(engineSym string, snap *orderbook.Snapshot) (firstID, finalID int64, bids, asks [][2]string, changed bool) {
	curBids, curAsks := snapMaps(snap)

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stateLocked(engineSym)

	if st.bids == nil { // first sighting: seed baseline, emit nothing.
		st.bids, st.asks = curBids, curAsks
		return 0, 0, nil, nil, false
	}

	bids = diffSide(st.bids, curBids)
	asks = diffSide(st.asks, curAsks)
	if len(bids) == 0 && len(asks) == 0 {
		return 0, 0, nil, nil, false
	}

	prevID := st.lastID
	st.lastID = prevID + 1
	st.bids, st.asks = curBids, curAsks
	return prevID + 1, st.lastID, bids, asks, true
}
