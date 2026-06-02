package emulator

import (
	"context"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// ArbHarness mirrors one underlying reference market into TWO engine
// instruments ("venues"), each with its own PriceShift, to manufacture a
// controllable cross-venue dislocation — a lab for testing arbitrage /
// relative-value strategies (PLAN §5 Phase 7). Feed both legs the same book
// frames via Feed; each leg shifts the frame by its configured PriceShift
// before updating its reference book, and Reconcile seeds each leg's (shifted)
// liquidity into its engine instrument. Drive the two shifts apart to open a
// closeable arbitrage; realign (identity shift) to close it. Both instruments
// must be registered with the engine's order book.
type ArbHarness struct {
	legs [2]*arbLeg
}

type arbLeg struct {
	instrument string
	shift      PriceShift
	ref        *reference.Book
	seeder     *Seeder
}

// NewArbHarness builds a two-venue harness. exchange is the reference-book
// exchange tag; depth caps the mirrored levels per side (<=0 = all). shiftA /
// shiftB are the per-venue price transforms (use the zero PriceShift{} for an
// undislocated venue).
func NewArbHarness(matcher Matcher, exchange string, depth int, instrA string, shiftA PriceShift, instrB string, shiftB PriceShift) *ArbHarness {
	if instrA == instrB {
		panic("emulator: arb harness needs two distinct instruments, got " + instrA)
	}
	h := &ArbHarness{}
	for i, lc := range []struct {
		inst  string
		shift PriceShift
	}{{instrA, shiftA}, {instrB, shiftB}} {
		ref := reference.NewBook(lc.inst, exchange)
		h.legs[i] = &arbLeg{
			instrument: lc.inst,
			shift:      lc.shift,
			ref:        ref,
			seeder:     NewSeeder(matcher, ref, Config{Instrument: lc.inst, DepthLevels: depth}),
		}
	}
	return h
}

// Feed applies one underlying book frame to both venues. Each leg shifts the
// frame by its own PriceShift and re-targets the frame to that leg's instrument
// before folding it into the leg's reference book. Trade frames are ignored
// (the harness models the resting book; tape replay is a separate concern).
func (h *ArbHarness) Feed(ev feed.Event) {
	if ev.Kind != feed.EventBook || ev.Book == nil {
		return
	}
	for _, leg := range h.legs {
		shifted := leg.shift.ApplyEvent(ev) // shifted copy (identity shift returns ev as-is)
		// Deep-copy the level slices before re-instrumenting: an identity-shift
		// leg aliases the caller's slices, and two legs share one input event —
		// so a shallow copy would have the legs (and the caller) share backing
		// arrays. Apply is read-only today, but we don't want to depend on that.
		b := *shifted.Book
		b.Instrument = leg.instrument
		b.Bids = append([]feed.LOBLevel(nil), b.Bids...)
		b.Asks = append([]feed.LOBLevel(nil), b.Asks...)
		leg.ref.Apply(&b)
	}
}

// SetShift changes a leg's price shift and reports whether a leg matched. The
// new shift takes effect on the next Feed (which re-derives that leg's
// reference from the underlying frame). A false return means instrument names
// no venue in this harness — almost always a wiring typo.
func (h *ArbHarness) SetShift(instrument string, s PriceShift) bool {
	for _, leg := range h.legs {
		if leg.instrument == instrument {
			leg.shift = s
			return true
		}
	}
	return false
}

// Reconcile seeds both venues' engine books from their (shifted) references.
func (h *ArbHarness) Reconcile(ctx context.Context) error {
	for _, leg := range h.legs {
		if _, err := leg.seeder.Reconcile(ctx); err != nil {
			return err
		}
	}
	return nil
}

// CrossArb reports the per-unit profit of the two cross-venue arbitrage
// directions between two venue books for the same asset:
//
//	buyBSellA = bidA - askB   (buy on B at its ask, sell on A at its bid)
//	buyASellB = bidB - askA   (buy on A at its ask, sell on B at its bid)
//
// A positive value is an exploitable arbitrage in that direction; <= 0 means
// none. A side with no bid or no ask contributes no opportunity (tested by
// presence of levels, not price sign, so it is correct for any price domain).
func CrossArb(a, b *orderbook.Snapshot) (buyBSellA, buyASellB decimal.Decimal) {
	if len(a.Bids) > 0 && len(b.Asks) > 0 {
		buyBSellA = a.BestBid.Sub(b.BestAsk)
	}
	if len(b.Bids) > 0 && len(a.Asks) > 0 {
		buyASellB = b.BestBid.Sub(a.BestAsk)
	}
	return
}
