package optmarket

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// Settler drives the EXPIRY SWEEP for the listed options (cash settlement).
//
// Every contract on this surface is CASH-settled in the quote asset at its
// settlement instant. The sweep, per contract expired on this pass (vanillas
// in listing order, then exotics):
//
//  1. CLAIMS the symbol (atomic; a settled symbol never settles twice within
//     the process — a double tick or a concurrent pass is a no-op),
//  2. reads the settlement value,
//  3. cancels every RESTING order on the contract (so nothing can fill past
//     the settlement instant and no reservation is pinned),
//  4. records the settlement and fires the settlement hook so the venue can
//     settle CASH for the client's position in the balance ledger.
//
// Settlement value:
//
//   - vanilla: its OWN intrinsic off the settlement index — max(index−K,0)
//     for a call, max(K−index,0) for a put — times the spec multiplier. The
//     max(·,0) makes a negative settlement impossible. A MISSING index is NOT
//     a fabricable 0 (index 0 would credit a put its full strike): the
//     contract stays UNSETTLED and is retried on the next pass.
//   - flow exotic: the terminal value of ITS OWN payoff, re-priced FRESH
//     through the attached Pricer at the settlement instant — never the
//     strike's intrinsic, never a vanilla leg. The claim is taken BEFORE the
//     (subprocess) price call and released if pricing fails, so a broken
//     payoff is retried without double-pricing and a slow engine never holds
//     the settlement lock.
//
// Post-pass drain: a place is not serialized with the sweep, so an order can
// rest (or even cross) a contract in the instant after it settled — on every
// pass the sweep re-cancels the rests of settled symbols so a late order
// cannot fill past the settlement instant or pin a reservation.
//
// The sweep is deterministic (order, index source, clock) and within-process
// idempotent per symbol.
type Settler struct {
	// Matcher is the slice of the matching engine the sweep needs: list the
	// resting orders of one instrument and cancel them.
	Matcher Matcher
	// Logf receives sweep diagnostics; nil means silent.
	Logf func(format string, args ...interface{})
}

// Matcher is the slice of the matching engine the expiry sweep drives — the
// same shape the binance edge's Engine interface carries, plus a resting-order
// listing, so tests can substitute a fake.
type Matcher interface {
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
	OpenOrders(instrument string) []*orderbook.Order
}

// Settlement is one completed expiry settlement. PerUnit is the CASH value of
// one contract unit at settlement (multiplier already folded in): what a
// holder of 1 unit receives in the quote asset.
type Settlement struct {
	Symbol   string
	IsExotic bool
	// Index is the settlement index print (point-in-time rule); 0 when it is
	// unavailable (exotics do not require one).
	Index float64
	// PerUnit is the settlement cash value of one contract unit.
	PerUnit float64
	TimeMs  int64
	// Multiplier is carried for the audit trail (already folded into PerUnit).
	Multiplier float64
}

// SettlementHook settles CASH for the client's position in one settled
// contract: it is called with the contract's held units (longQty, pre-
// multiplier) and must credit longQty·PerUnit into the QUOTE asset and
// BURN the contract asset (expired units are no longer a position). The
// single-account emulated model has no net-short positions — a sell can
// never exceed the held balance — so only the long side is plumbed.
type SettlementHook func(rec Settlement, longQty float64)

// positionsFunc reports the client's held contract units for one symbol.
type positionsFunc func(sym string) (long float64)

// Settle runs ONE expiry-sweep pass as of the market clock and returns the
// settlements completed on this pass (vanillas in listing order, then
// exotics). Safe to call from a ticker: within the process a settled contract
// never settles twice, and unsettled (missing index / unpricable payoff)
// contracts are retried on every pass.
func (m *Market) Settle(ctx context.Context, s *Settler, positions positionsFunc, onSettle SettlementHook) []Settlement {
	if s == nil {
		return nil
	}
	now := m.now()
	var out []Settlement

	// Vanillas in listing order.
	for _, in := range m.order {
		sym := in.Symbol()
		if !in.IsExpired(now) || !m.claim(sym) {
			continue
		}
		idx, ok := m.index(in.Underlying)
		if !ok {
			m.settleLogf(s, "%s: no settlement index for %q at expiry — LEFT UNSETTLED", sym, in.Underlying)
			m.unclaim(sym)
			continue
		}
		out = append(out, m.finishSettle(ctx, s, sym, false, idx, in.SettlePrice(idx), in.Spec.EffectiveMultiplier(), positions, onSettle))
	}

	// Exotics in listing order. The claim precedes the (subprocess) price
	// call; failure unclaims so the next pass retries.
	for _, sym := range m.exoticOrder {
		ex := m.exotics[sym]
		if now.Before(ex.Expiry) || !m.claim(sym) {
			continue
		}
		perUnit, err := m.settleExoticPrice(sym)
		if err != nil {
			m.settleLogf(s, "%s: payoff does not price at settlement — LEFT UNSETTLED: %v", sym, err)
			m.unclaim(sym)
			continue
		}
		idx, _ := m.index(ex.Underlying) // informational; not required
		out = append(out, m.finishSettle(ctx, s, sym, true, idx, perUnit, 1, positions, onSettle))
	}

	// Post-pass drain: cancel rests of everything settled (this or an earlier
	// pass) so an order racing in around a settlement cannot fill past the
	// settlement instant or hold a reservation.
	for _, sym := range m.settledKeys() {
		m.cancelResting(ctx, sym, s)
	}
	return out
}

// settleOne cancels rests, records (the caller CLAIMED the symbol), and fires
// the cash hook for one contract.
func (m *Market) finishSettle(ctx context.Context, s *Settler, sym string, isExotic bool, idx, perUnit, mult float64, positions func(string) float64, onSettle SettlementHook) Settlement {
	m.cancelResting(ctx, sym, s)
	rec := Settlement{
		Symbol:     sym,
		IsExotic:   isExotic,
		Index:      idx,
		PerUnit:    perUnit,
		TimeMs:     m.now().UnixMilli(),
		Multiplier: mult,
	}
	m.settleMu.Lock()
	m.settlements = append(m.settlements, rec)
	m.settleMu.Unlock()
	if positions != nil && onSettle != nil {
		if l := positions(sym); l > 0 {
			onSettle(rec, l)
		}
	}
	return rec
}

// claim atomically marks sym settled; false if it already was.
func (m *Market) claim(sym string) bool {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	if m.settled[sym] {
		return false
	}
	m.settled[sym] = true
	return true
}

// unclaim releases a claimed symbol so the next pass retries it (the
// settlement value was unavailable: no index / unpricable payoff).
func (m *Market) unclaim(sym string) {
	m.settleMu.Lock()
	delete(m.settled, sym)
	m.settleMu.Unlock()
}

// settledKeys lists claimed symbols (snapshot).
func (m *Market) settledKeys() []string {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	out := make([]string, 0, len(m.settled))
	for sym := range m.settled {
		out = append(out, sym)
	}
	return out
}

// cancelResting removes every resting order on the symbol so nothing can fill
// past the settlement instant. Best-effort and bounded: a cancel error is
// logged and the sweep continues; a later pass retries any survivor.
func (m *Market) cancelResting(ctx context.Context, sym string, s *Settler) {
	if s == nil || s.Matcher == nil {
		return
	}
	for _, o := range s.Matcher.OpenOrders(sym) {
		if _, err := s.Matcher.CancelOrder(ctx, sym, o.ID); err != nil {
			m.settleLogf(s, "%s: cancel %s: %v (continuing)", sym, o.ID, err)
		}
	}
}

// settleExoticPrice prices an exotic's ACTUAL payoff freshly at the terminal
// instant — the T=0, spot=index evaluation of the contract the venue sold.
// Unlike a mark lookup (cached, error-tolerant), a failure here is returned
// to the caller, which leaves the contract unsettled: no value is fabricated.
// A missing index is an ERROR here too (not a fabricated spot=0), because
// fabricating the terminal spot is the same class of mistake as fabricating
// the vanilla intrinsic: the payoff depends on that spot.
func (m *Market) settleExoticPrice(sym string) (float64, error) {
	if m.reg == nil {
		return 0, fmt.Errorf("no pricer attached")
	}
	ex, ok := m.exotics[sym]
	if !ok {
		return 0, fmt.Errorf("unknown exotic %q", sym)
	}
	px, ok := m.index(ex.Underlying)
	if !ok || px <= 0 {
		return 0, fmt.Errorf("no settlement index for underlying %q", ex.Underlying)
	}
	p := m.sharedModel
	p.Symbol = sym
	p.Script = ex.Payoff
	p.ScriptFile = ex.PayoffFile
	if ex.Model != "" {
		p.Model = ex.Model
	}
	if ex.Vol > 0 {
		p.Vol = ex.Vol
	} else if p.Vol <= 0 {
		p.Vol = m.smileFor(ex.Underlying).Vol(ex.Strike, px, 0)
	}
	p.Spot = px
	p.ExpiryYears = 0 // the terminal instant
	p.Strike = 0      // the payoff script defines the instrument
	p.Side = ""
	pc, ok := m.reg.Get(m.pricerName)
	if !ok {
		return 0, fmt.Errorf("pricer %q not registered", m.pricerName)
	}
	res, err := pc.Price(context.Background(), p)
	if err != nil {
		return 0, fmt.Errorf("pricer %q: %w", m.pricerName, err)
	}
	if math.IsNaN(res.Price) || math.IsInf(res.Price, 0) || !(res.Price >= 0) {
		return 0, fmt.Errorf("pricer %q returned a non-finite or negative terminal value %v", m.pricerName, res.Price)
	}
	return res.Price, nil
}

// Settlements returns the settlements completed so far, in completion order.
func (m *Market) Settlements() []Settlement {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	return append([]Settlement(nil), m.settlements...)
}

// IsSettled reports whether the symbol has already been settled by the sweep.
func (m *Market) IsSettled(sym string) bool {
	m.settleMu.Lock()
	defer m.settleMu.Unlock()
	return m.settled[sym]
}

func (m *Market) settleLogf(s *Settler, format string, args ...interface{}) {
	if s != nil && s.Logf != nil {
		s.Logf(format, args...)
	}
}

var _ = sync.Mutex{} // settleMu lives on Market
