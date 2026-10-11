package optmarket

import (
	"math"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// ContractSpec are the settlement- and exercise-side terms of one listed
// option, split out of the pricing terms. It is intentionally narrow: this
// surface emulates one well-defined contract shape, and the fields exist so
// the shape is EXPLICIT and machine-consumable (the exchangeInfo spec and the
// expiry cash-settle both read them) rather than implied by package-prose
// conventions.
type ContractSpec struct {
	// Exercise is the exercise style. Only "european" is implemented:
	// no early exercise, no American payoff.
	Exercise string
	// Settlement is the settlement mode. Only "cash" is implemented:
	// intrinsic settles in the quote asset per unit (USDT here); physical
	// (underlying) delivery is out of scope.
	Settlement string
	// IndexRule is how the settlement index is determined. Only
	// "point_in_time" is implemented: the index print at the expiry instant.
	// Deribit-style TWAP settlement is out of scope (noted, not faked).
	IndexRule string
	// Multiplier scales the settled cash amount per contract unit. 1 =
	// per-coin settlement, like linear crypto options; 0 means 1.
	Multiplier float64
}

// DefaultContractSpec is the default listing for every instrument on this
// surface: European exercise, cash settlement, point-in-time index, 1:1
// multiplier. NewInstrument stamps it in.
var DefaultContractSpec = ContractSpec{
	Exercise:   "european",
	Settlement: "cash",
	IndexRule:  "point_in_time",
	Multiplier: 1,
}

// EffectiveMultiplier is the multiplier with a zero (unset) defaulting to 1.
func (c ContractSpec) EffectiveMultiplier() float64 {
	if c.Multiplier > 0 {
		return c.Multiplier
	}
	return 1
}

// Instrument is one European, cash-settled option: pricing terms plus the
// settlement ContractSpec.
type Instrument struct {
	Spec ContractSpec
	// Coin is the symbol prefix (e.g. "BTC"); Underlying is the index pair the
	// spot price is read from (e.g. "BTCUSDT"). Quote is the cash-settlement
	// asset (e.g. "USDT").
	Coin       string
	Underlying string
	Quote      string
	Strike     float64
	Kind       options.Kind
	// Expiry is the settlement instant (date @ 08:00 UTC).
	Expiry time.Time
}

// NewInstrument builds an instrument from its parts. `date` is the calendar
// expiry day; the settlement instant is set to 08:00 UTC on that day. The
// spec defaults to DefaultContractSpec.
func NewInstrument(coin, underlying, quote string, strike float64, kind options.Kind, date time.Time) Instrument {
	y, m, d := date.UTC().Date()
	return Instrument{
		Spec:       DefaultContractSpec,
		Coin:       coin,
		Underlying: underlying,
		Quote:      quote,
		Strike:     strike,
		Kind:       kind,
		Expiry:     time.Date(y, m, d, expiryHourUTC, 0, 0, 0, time.UTC),
	}
}

// SettlePrice is the CASH settlement price of one contract unit at index:
//   - call: max(index - strike, 0) * multiplier
//   - put:  max(strike - index, 0) * multiplier
//
// It is the intrinsic value under this surface's cash-settle spec — the
// expiry sweep settles open option inventory at exactly this number.
func (in Instrument) SettlePrice(index float64) float64 {
	m := in.Spec.EffectiveMultiplier()
	switch in.Kind {
	case options.Call:
		return math.Max(index-in.Strike, 0) * m
	default: // Put
		return math.Max(in.Strike-index, 0) * m
	}
}
