package emulator

import (
	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// PriceShift transforms a venue's prices to manufacture cross-venue
// dislocations — a controlled lab for arbitrage / relative-value strategy
// testing (PLAN §5 Phase 7). The transform is
//
//	shifted = price * Scale * (1 + OffsetBps/10000)
//
// Both controls compose. The zero value (OffsetBps=0, Scale=0) is the
// identity: Scale=0 is treated as 1, so an un-configured shift is a no-op.
// Quantities are never touched — only prices move.
type PriceShift struct {
	OffsetBps float64
	Scale     float64
}

// NewPriceShift normalizes a raw config into a PriceShift, mapping Scale=0 to
// 1 so the zero value (and an omitted yaml block) is the identity transform.
func NewPriceShift(offsetBps, scale float64) PriceShift {
	if scale == 0 {
		scale = 1
	}
	p := PriceShift{OffsetBps: offsetBps, Scale: scale}
	// A non-positive or non-finite factor (e.g. offset_bps ≤ -10000, or a
	// negative scale) would zero or sign-flip every price — meaningless and
	// destructive. Treat such a config as the identity rather than wreck the book.
	if f := p.factor(); !finite(f) || f <= 0 {
		return PriceShift{OffsetBps: 0, Scale: 1}
	}
	return p
}

// factor is the multiplicative price factor Scale*(1+OffsetBps/10000), with
// Scale=0 normalized to 1 (so a literal zero-value PriceShift is identity).
func (p PriceShift) factor() float64 {
	scale := p.Scale
	if scale == 0 {
		scale = 1
	}
	return scale * (1 + p.OffsetBps/10000)
}

// IsIdentity reports whether the shift leaves every price unchanged (factor
// == 1), so callers can skip the transform entirely for zero overhead.
func (p PriceShift) IsIdentity() bool {
	return p.factor() == 1
}

// shiftPriceDecimal shifts a single (priceDecimal, price) pair in exact
// decimal: the venue's PriceDecimal string is authoritative (the reference
// book parses it preferentially), so we parse it — falling back to
// FromFloat(price) when empty/unparseable — multiply by the float factor via
// Decimal.MulFloat, and return both the new string and float forms. price is
// assumed finite (feed floats are guarded upstream); MulFloat goes through
// FromFloat, so a non-finite product would panic — the factor is finite and
// the venue prices are finite, so the product is finite.
func shiftPriceDecimal(priceDecimal string, price, factor float64) (string, float64) {
	d, err := decimal.Parse(priceDecimal)
	if err != nil {
		d = decimal.FromFloat(price)
	}
	shifted := d.MulFloat(factor)
	return shifted.StringPrec(decimal.ScaleDigits), shifted.Float64()
}

// ApplyEvent returns a copy of ev with every price shifted: each book level's
// price (Bids and Asks) and every trade's price. Both the float Price field
// and the PriceDecimal string are updated together — the reference book parses
// PriceDecimal preferentially, so shifting only the float would be silently
// ignored. Quantities are unchanged. The input event's slices are not mutated
// in place (the caller may reuse them). If the shift is the identity, ev is
// returned unchanged.
func (p PriceShift) ApplyEvent(ev feed.Event) feed.Event {
	if p.IsIdentity() {
		return ev
	}
	f := p.factor()

	switch ev.Kind {
	case feed.EventTrade:
		if ev.Trade == nil {
			return ev
		}
		t := *ev.Trade // copy
		t.PriceDecimal, t.Price = shiftPriceDecimal(t.PriceDecimal, t.Price, f)
		ev.Trade = &t
	case feed.EventBook:
		if ev.Book == nil {
			return ev
		}
		b := *ev.Book // copy
		b.Bids = shiftLevels(b.Bids, f)
		b.Asks = shiftLevels(b.Asks, f)
		ev.Book = &b
	}
	return ev
}

// shiftLevels returns a new slice with every level's price shifted by factor;
// the input slice is left untouched so the caller can reuse it.
func shiftLevels(in []feed.LOBLevel, factor float64) []feed.LOBLevel {
	if in == nil {
		return nil
	}
	out := make([]feed.LOBLevel, len(in))
	for i, l := range in {
		l.PriceDecimal, l.Price = shiftPriceDecimal(l.PriceDecimal, l.Price, factor)
		out[i] = l
	}
	return out
}
