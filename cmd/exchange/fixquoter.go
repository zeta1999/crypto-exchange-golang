package main

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// defaultQuoteSpread is the bid/offer half-spread around the exotic's fair
// value, in bps of the fair value itself (10 = 0.10% each side). Config can
// override.
const defaultQuoteSpreadBps = 10.0

// fixQuoter adapts the pricer plugin into the FIX edge's Quoter. Each exotic
// instrument is priced by the registered backend AGAINST ITS ACTUAL PAYOFF
// (an inline Aria script or a payoff_file path taken from config), using the
// live engine-book mid as the spot. There is exactly ONE fair value per
// instrument — never a synthetic pair. The executable two-sided quote is then
//
//	bid   = fair · (1 − w)
//	offer = fair · (1 + w)
//
// where w = spreadBps/10000 (e.g. 10 bps → 0.001). bid ≤ offer trivially
// because w ≥ 0. The payoff — barrier, Asian, autocall, lookback, digital, …
// — is whatever the operator configured; the FIX edge stays blind to it.
type fixQuoter struct {
	reg      *pricer.Registry
	pricerNm string
	book     *orderbook.OrderBook

	// exotics maps engine symbol (as resolved by the FIX SymbolResolver) to
	// the payoff + model knobs for that instrument.
	exotics map[string]exoticSpec
	// spotFrom maps exotic engine symbol -> engine symbol whose book mid is
	// the pricing spot. Absent → the exotic's own book.
	spotFrom map[string]string

	// sharedModel carries the per-quote defaults; per-exotic specs may
	// override Vol / Model.
	sharedModel  pricer.Params
	spreadWeight decimal.Decimal // w = spreadBps/10000, decimal
}

type exoticSpec struct {
	script     string // exactly one of script / scriptFile non-empty (enforced at build)
	scriptFile string
	vol        float64 // 0 = inherit sharedModel.Vol
	model      string  // "" = inherit sharedModel.Model
}

// newFixQuoter wires the quoter: one spec per configured exotic, shared
// pricing model from the quotes config, the configured half-spread.
// Validation is fail-fast: an exotic with neither Payoff nor PayoffFile is a
// misconfiguration (the operator would get a runtime quote-reject for every
// order and it's not diagnosable from the log); an exotic with BOTH is
// rejected at Price time but is also caught here.
func newFixQuoter(
	reg *pricer.Registry,
	pricerName string,
	book *orderbook.OrderBook,
	exotics []config.FIXExotic,
	spotFrom map[string]string,
	spreadBp float64,
	shared pricer.Params,
) (*fixQuoter, error) {
	if spreadBp <= 0 {
		spreadBp = defaultQuoteSpreadBps
	}
	specs := make(map[string]exoticSpec, len(exotics))
	for _, e := range exotics {
		eng := strings.TrimSpace(e.Engine)
		if eng == "" {
			return nil, fmt.Errorf("fixquoter: exotic entry with empty engine")
		}
		if e.Payoff == "" && e.PayoffFile == "" {
			return nil, fmt.Errorf("fixquoter: exotic %q has neither payoff nor payoff_file", eng)
		}
		if e.Payoff != "" && e.PayoffFile != "" {
			return nil, fmt.Errorf("fixquoter: exotic %q sets both payoff and payoff_file (pick one)", eng)
		}
		if _, dup := specs[eng]; dup {
			return nil, fmt.Errorf("fixquoter: duplicate exotic %q", eng)
		}
		specs[eng] = exoticSpec{
			script:     e.Payoff,
			scriptFile: e.PayoffFile,
			vol:        e.Vol,
			model:      e.Model,
		}
	}
	return &fixQuoter{
		reg: reg, pricerNm: pricerName, book: book,
		exotics: specs, spotFrom: spotFrom,
		sharedModel:  shared,
		spreadWeight: decimal.FromFloat(spreadBp / 10000.0),
	}, nil
}

// IsExotic reports whether orders on the instrument must go through the
// quote flow per the FIX edge's Quoter contract.
func (q *fixQuoter) IsExotic(engineSymbol string) bool {
	_, ok := q.exotics[engineSymbol]
	return ok
}

// Quote prices the exotic's ACTUAL configured payoff via the registered
// pricer (spot = live mid of the funded underlying per spotFrom) and returns
// executable bid/ask decimals bracketing the returned fair value.
func (q *fixQuoter) Quote(ctx context.Context, engineSymbol string, qty decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	spec, ok := q.exotics[engineSymbol]
	if !ok {
		return decimal.Zero, decimal.Zero, fmt.Errorf("no exotic configured for %s", engineSymbol)
	}

	spotSym := engineSymbol
	if s, ok := q.spotFrom[engineSymbol]; ok {
		spotSym = s
	}
	snap, err := q.book.Snapshot(spotSym)
	if err != nil {
		return decimal.Zero, decimal.Zero, fmt.Errorf("no book: %w", err)
	}
	if snap.BestBid.Sign() <= 0 || snap.BestAsk.Sign() <= 0 {
		return decimal.Zero, decimal.Zero, fmt.Errorf("book empty for %s", engineSymbol)
	}
	// Mid of the funding instrument is the MC spot ("S" in the .aria script).
	mid := snap.BestBid.Add(snap.BestAsk).Div(decimal.FromInt(2))

	pc, ok := q.reg.Get(q.pricerNm)
	if !ok {
		return decimal.Zero, decimal.Zero, fmt.Errorf("no pricer named %q registered", q.pricerNm)
	}

	p := q.sharedModel
	p.Symbol = engineSymbol
	p.Spot = mid.Float64()
	p.Script = spec.script
	p.ScriptFile = spec.scriptFile
	if spec.vol > 0 {
		p.Vol = spec.vol
	}
	if spec.model != "" {
		p.Model = spec.model
	}
	// Strike/Side are ignored by every payoff path this edge uses; the payoff
	// itself is the script. Clear them so a future scriptless vanilla inference
	// in the backend cannot pick them up and fabricate a payoff.
	p.Strike = 0
	p.Side = ""

	res, err := pc.Price(ctx, p)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if math.IsNaN(res.Price) || math.IsInf(res.Price, 0) {
		return decimal.Zero, decimal.Zero, fmt.Errorf("pricer returned non-finite price %v", res.Price)
	}
	fair := decimal.FromFloat(res.Price)
	if fair.Sign() <= 0 {
		// A non-positive MC price is almost always a script that pays 0
		// everywhere (deep-OTM or knocked-out barrier on this spot). Do not
		// quote "0" — that would be a free instrument. Surface the error.
		return decimal.Zero, decimal.Zero, fmt.Errorf("pricer returned non-positive fair value %s for %s (payoff may be identically zero at this spot)", fair, engineSymbol)
	}

	// Bracket the single fair value by the configured half-spread (bps of
	// fair). bid = fair·(1−w), offer = fair·(1+w). Because w ≥ 0 the pair
	// is intrinsically uncrossed; we do not clip either side to 0 — the
	// fair > 0 check above already guarantees bid > 0 when w < 1 (and w is
	// a bps fraction, never near 1).
	halfBps := fair.Mul(q.spreadWeight)
	bid := fair.Sub(halfBps)
	offer := fair.Add(halfBps)
	if bid.Sign() < 0 { // pathological defensive only; real w < 1
		bid = decimal.FromFloat(1e-9)
	}
	return bid, offer, nil
}
