package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// defaultQuoteSpread is the bid/offer spread around fair value, in bps of spot
// (10 = one-tenth of a percent each side). Config can override.
const defaultQuoteSpreadBps = 10.0

// spreadBpsFromConfig parses a config spread (bps) or falls back to the default.
func quoteSpreadBps(f float64) float64 {
	if f > 0 {
		return f
	}
	return defaultQuoteSpreadBps
}

// fixQuoter adapts the pricer plugin into the FIX edge's Quoter: it reads ONE
// vanilla option contract per instrument (strike = current book mid, expiry
// from the instrument record), prices both the call and the put through the
// registered backend, and quotes:
//
//	bid   = S - K + put
//	offer = S - K + call
//
// which is put-call parity (so bid ≤ offer always, even off-model), then
// widens each leg by half the configured spread. Spot S is the live engine
// book mid, so quotes track the emulated market.
type fixQuoter struct {
	reg      *pricer.Registry
	pricerNm string
	book     *orderbook.OrderBook
	exotics  map[string]bool
	spotFrom map[string]string // exotic engine symbol -> instrument whose book mid is the pricing spot
	spreadBp float64
	model    pricer.Params // shared model knobs (vol/paths/seed/T/Model)
}

// newFixQuoter builds a quoter for the configured exotics, using the pricer
// named `pricerName` from the shared registry. spotFrom maps an exotic to the
// instrument whose live book mid is its pricing spot (nil = its own book).
func newFixQuoter(reg *pricer.Registry, pricerName string, book *orderbook.OrderBook,
	exotics []string, spotFrom map[string]string, spreadBp float64, model pricer.Params) *fixQuoter {
	return &fixQuoter{
		reg: reg, pricerNm: pricerName, book: book, spreadBp: spreadBp, model: model,
		exotics: exoticSet(exotics), spotFrom: spotFrom,
	}
}

func exoticSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

func (q *fixQuoter) IsExotic(engineSymbol string) bool { return q.exotics[engineSymbol] }

// Quote prices one instrument's vanilla call/put at the live book mid and
// returns executable bid/ask decimals. The pricing spot is the mid of the
// instrument mapped in spotFrom (or the instrument's own book when unset), so
// a quote-only exotic can track a funded underlying.
func (q *fixQuoter) Quote(ctx context.Context, engineSymbol string, qty decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
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
	// Mid (S) and strike (K) from the live book, so the option stays at/around
	// ATM and the quote tracks the emulated market.
	s := snap.BestBid.Add(snap.BestAsk).Div(decimal.FromInt(2))
	k := s

	call, err := q.priceLeg(ctx, engineSymbol, k, "C", s)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	put, err := q.priceLeg(ctx, engineSymbol, k, "P", s)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}

	half := s.Mul(decimal.FromFloat(q.spreadBp * 0.5 / 10000.0))
	bid := s.Sub(k).Add(put).Add(half)
	offer := s.Sub(k).Add(call).Sub(half)
	if bid.Sign() < 0 {
		bid = decimal.Zero
	}
	if bid.Gt(offer) { // safety: parity should prevent this, but never cross
		offer = bid
	}
	return bid, offer, nil
}

// priceLeg prices one C or P leg (fair vanilla value) via the registered
// pricer for the instrument. The payoff is a minimal European at K.
func (q *fixQuoter) priceLeg(ctx context.Context, engineSymbol string, k decimal.Decimal, side string, s decimal.Decimal) (decimal.Decimal, error) {
	pc, ok := q.reg.Get(q.pricerNm)
	if !ok {
		return decimal.Zero, fmt.Errorf("no pricer named %q registered", q.pricerNm)
	}

	params := q.model
	params.Symbol = engineSymbol
	params.Spot = s.Float64()
	params.Strike = k.Float64()
	params.Side = side
	params.Script = vanillaAriaScript(k.Float64(), side == "P")
	params.ScriptFile = ""
	res, err := pc.Price(ctx, params)
	if err != nil {
		return decimal.Zero, err
	}
	if math.IsNaN(res.Price) || math.IsInf(res.Price, 0) || res.Price < 0 {
		return decimal.Zero, fmt.Errorf("bad price %v", res.Price)
	}
	return decimal.FromFloat(res.Price), nil
}

// vanillaAriaScript is the minimal Aria contract for a European plain option
// (the saga plugin's documented vanilla shape).
func vanillaAriaScript(strike float64, put bool) string {
	body := fmt.Sprintf("max(S - %s, 0.0)", trimNum(strike))
	if put {
		body = fmt.Sprintf("max(%s - S, 0.0)", trimNum(strike))
	}
	kind := "Call"
	if put {
		kind = "Put"
	}
	return fmt.Sprintf(`contract %s { let S = spot("X") one(KRW) |> scale(%s) |> when(2026-01-01) }`, kind, body)
}

// trimNum formats without trailing zeros / exponential noise.
func trimNum(v float64) string {
	t := strconv.FormatFloat(v, 'f', -1, 64)
	return t
}
