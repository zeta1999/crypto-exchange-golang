package optmarket

import (
	"context"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// fakeMatcher is a matching-engine fake for the expiry sweep: a map of
// resting orders per instrument; OpenOrders lists them, CancelOrder removes.
type fakeMatcher struct {
	rest     map[string][]*orderbook.Order
	canceled []string
}

func newFakeMatcher() *fakeMatcher {
	return &fakeMatcher{rest: map[string][]*orderbook.Order{}}
}

func (f *fakeMatcher) restOrder(sym, id string, side orderbook.Side) {
	f.rest[sym] = append(f.rest[sym], &orderbook.Order{
		ID: id, Instrument: sym, Side: side, Price: decimal.FromInt(1), Volume: decimal.FromInt(1),
	})
}

func (f *fakeMatcher) CancelOrder(_ context.Context, inst, id string) (*orderbook.Order, error) {
	for i, o := range f.rest[inst] {
		if o.ID == id {
			f.rest[inst] = append(f.rest[inst][:i], f.rest[inst][i+1:]...)
			f.canceled = append(f.canceled, id)
			return o, nil
		}
	}
	return nil, orderbook.ErrOrderNotFound
}

func (f *fakeMatcher) OpenOrders(inst string) []*orderbook.Order { return f.rest[inst] }

// settleMarket: a BTC chain (3 strikes, C+P) plus one barrier exotic, with
// the clock parked one second AFTER the settlement instant (2026-12-31
// 08:00 UTC) so one sweep pass settles the whole list. The mock pricer
// returns a fixed exotic terminal value (7.25) — settlement re-prices the
// payoff freshly (T=0) instead of reading the TTL-cached mark.
func settleMarket(t *testing.T, index func(string) (float64, bool)) (*Market, *mockPricer) {
	t.Helper()
	exp := time.Date(2026, 12, 31, expiryHourUTC, 0, 0, 0, time.UTC)
	now := exp.Add(1 * time.Second)
	m := NewMarket(func() time.Time { return now }, index, 0.03, 0.02, 0.02, 0.5)
	m.SetSurface("BTCUSDT", VolSurface{ATMVol: 0.65})
	for _, k := range []float64{40000, 50000, 60000} {
		m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", k, options.Call, exp))
		m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", k, options.Put, exp))
	}
	reg := pricer.NewRegistry()
	mock := &mockPricer{price: 7.25}
	reg.Register(mock)
	if err := m.AttachPricer(reg, "mock", pricer.Params{Vol: 0.30, NumPaths: 1024, NumSteps: 24, Seed: 1},
		time.Second, 0, 0); err != nil {
		t.Fatal(err)
	}
	ex := NewExoticInstrument("BTC", "BTCUSDT", "USDT", 50000, options.Call, exp, "BTC-261231-50000-BAR")
	ex.PayoffFile = "payoffs/barrier_down_out_call.aria"
	if err := m.AddExotic(ex); err != nil {
		t.Fatal(err)
	}
	return m, mock
}

func TestSettle_CashAtIntrinsicAndPayoff(t *testing.T) {
	fm := newFakeMatcher()
	fm.restOrder("BTC-261231-50000-C", "o1", orderbook.SideBuy)
	fm.restOrder("BTC-261231-40000-C", "o2", orderbook.SideSell)

	m, mock := settleMarket(t, func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 55000, true
		}
		return 0, false
	})
	// Accumulate the cash the venue must settle for the client positions
	// (long-only: held units — the single-account model has no net shorts).
	var got float64
	hook := func(rec Settlement, l float64) {
		got += l * rec.PerUnit
	}
	positions := map[string]float64{
		"BTC-261231-50000-C":   2, // long 2 ATM calls
		"BTC-261231-40000-C":   3, // long 3 deep-ITM calls
		"BTC-261231-50000-BAR": 1, // long 1 exotic
	}
	pos := func(sym string) float64 { return positions[sym] }

	settled := m.Settle(context.Background(), &Settler{Matcher: fm}, pos, hook)

	// 6 vanillas + 1 exotic, all in this one pass.
	if len(settled) != 7 {
		t.Fatalf("want 7 settlements, got %d: %+v", len(settled), settled)
	}
	by := map[string]Settlement{}
	for _, s := range settled {
		by[s.Symbol] = s
	}
	// Intrinsic at index 55000:
	//   calls: 60k OTM 0, 50k 5000, 40k 15000
	//   puts:  40k/50k OTM 0, 60k ITM 5000
	if by["BTC-261231-60000-C"].PerUnit != 0 {
		t.Errorf("OTM 60k call perUnit %v want 0", by["BTC-261231-60000-C"].PerUnit)
	}
	if by["BTC-261231-50000-C"].PerUnit != 5000 {
		t.Errorf("50k call perUnit %v want 5000", by["BTC-261231-50000-C"].PerUnit)
	}
	if by["BTC-261231-40000-C"].PerUnit != 15000 {
		t.Errorf("40k call perUnit %v want 15000", by["BTC-261231-40000-C"].PerUnit)
	}
	if by["BTC-261231-50000-P"].PerUnit != 0 {
		t.Errorf("OTM put must settle at 0, got %v", by["BTC-261231-50000-P"].PerUnit)
	}
	if by["BTC-261231-60000-P"].PerUnit != 5000 {
		t.Errorf("ITM 60k put perUnit %v want 5000", by["BTC-261231-60000-P"].PerUnit)
	}
	if !by["BTC-261231-50000-BAR"].IsExotic || by["BTC-261231-50000-BAR"].PerUnit != 7.25 {
		t.Errorf("exotic must settle at the fresh payoff terminal 7.25: %+v", by["BTC-261231-50000-BAR"])
	}
	if by["BTC-261231-50000-BAR"].Index != 55000 {
		t.Errorf("settlement record carries the index: %v", by["BTC-261231-50000-BAR"].Index)
	}
	// The sweep canceled the resting orders on the settled instruments.
	wantCancel := []string{"o2", "o1"} // 40000-C (o2) settles before 50000-C (o1)
	if len(fm.canceled) != len(wantCancel) {
		t.Fatalf("rests not canceled: %v (want %v)", fm.canceled, wantCancel)
	}
	for i, id := range fm.canceled {
		if id != wantCancel[i] {
			t.Errorf("canceled[%d]=%s want %s (listing order)", i, id, wantCancel[i])
		}
	}
	// The settlement hook cashed the positions (long-only held units):
	//   long 2 x 5000  = +10000
	//   long 3 x 15000 = +45000
	//   long 1 x 7.25  =  +7.25
	want := 2*5000.0 + 3*15000.0 + 1*7.25
	if got != want {
		t.Errorf("cash delta %v want %v", got, want)
	}
	// The terminal re-price really is a T=0 fresh call (not the cached mark).
	if mock.calls < 1 {
		t.Errorf("exotic terminal must re-price through the pricer")
	}
	if mock.last.ExpiryYears != 0 {
		t.Errorf("terminal re-price ExpiryYears %v want 0", mock.last.ExpiryYears)
	}
}

func TestSettle_IdempotentAndDeterministicOrder(t *testing.T) {
	m, _ := settleMarket(t, func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 51000, true
		}
		return 0, false
	})
	s1 := m.Settle(context.Background(), &Settler{}, nil, nil)
	s2 := m.Settle(context.Background(), &Settler{}, nil, nil)
	if len(s1) != 7 || len(s2) != 0 {
		t.Fatalf("idempotency broken: pass1=%d pass2=%d", len(s1), len(s2))
	}
	wantOrder := []string{
		"BTC-261231-40000-C", "BTC-261231-40000-P",
		"BTC-261231-50000-C", "BTC-261231-50000-P",
		"BTC-261231-60000-C", "BTC-261231-60000-P",
		"BTC-261231-50000-BAR",
	}
	if len(s1) != len(wantOrder) {
		t.Fatalf("count %d", len(s1))
	}
	for i, s := range s1 {
		if s.Symbol != wantOrder[i] {
			t.Errorf("settled[%d]=%s want %s (listing order, exotics last)", i, s.Symbol, wantOrder[i])
		}
	}
	if len(m.Settlements()) != 7 {
		t.Errorf("audit trail %d want 7", len(m.Settlements()))
	}
	if !m.IsSettled("BTC-261231-50000-BAR") {
		t.Error("exotic not marked settled")
	}
}

func TestSettle_BeforeExpiryNothing(t *testing.T) {
	// Clock before the settlement instant: nothing is expired yet.
	exp := time.Date(2026, 12, 31, expiryHourUTC, 0, 0, 0, time.UTC)
	m := NewMarket(func() time.Time { return exp.Add(-time.Hour) },
		func(u string) (float64, bool) { return 50000, true }, 0.03, 0.02, 0.02, 0.5)
	for _, k := range []float64{40000, 50000} {
		m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", k, options.Call, exp))
	}
	if s := m.Settle(context.Background(), &Settler{}, nil, nil); len(s) != 0 {
		t.Errorf("nothing may settle before expiry: %+v", s)
	}
}

func TestSettle_NegativeIntrinsicImpossible(t *testing.T) {
	// Deep-OTM at expiry: intrinsic 0, never negative.
	in := NewInstrument("BTC", "BTCUSDT", "USDT", 60000, options.Call, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	for _, idx := range []float64{1, 0, 59999.999} {
		if v := in.SettlePrice(idx); v != 0 {
			t.Errorf("OTM call intrinsic at index %v = %v, want 0", idx, v)
		}
	}
	put := NewInstrument("BTC", "BTCUSDT", "USDT", 40000, options.Put, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if v := put.SettlePrice(20000); v != 20000 {
		t.Errorf("ITM put intrinsic (index 20k < K 40k) %v want 20000", v)
	}
	if v := put.SettlePrice(40000); v != 0 {
		t.Errorf("ATM put intrinsic %v want 0", v)
	}
}

func TestSettle_RaceSurvivorIsDrained(t *testing.T) {
	// An order that reaches the book AFTER a pass settled the symbol (a place
	// racing the sweep) must be canceled by the NEXT pass — it cannot fill
	// past the settlement instant or pin a reservation.
	fm := newFakeMatcher()
	m, _ := settleMarket(t, func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 51000, true
		}
		return 0, false
	})
	sym := "BTC-261231-40000-C"
	if recs := m.Settle(context.Background(), &Settler{Matcher: fm}, nil, nil); len(recs) != 7 {
		t.Fatalf("pass1: %d", len(recs))
	}
	fm.restOrder(sym, "racy", orderbook.SideBuy) // rest ON the book after settlement
	if recs := m.Settle(context.Background(), &Settler{Matcher: fm}, nil, nil); len(recs) != 0 {
		t.Fatalf("pass2 must settle nothing: %d", len(recs))
	}
	if len(fm.rest[sym]) != 0 {
		t.Errorf("racy survivor not drained: %+v", fm.rest[sym])
	}
	if len(fm.canceled) == 0 {
		t.Error("no cancel issued for the survivor")
	}
}

func TestSettle_NoIndexIsNotAFabricatedZero(t *testing.T) {
	// A missing settlement index must NOT be turned into index=0: that would
	// credit a put its FULL strike (SettlePrice(0) = K for a put). The
	// contract stays UNSETTLED, is retried, and settles correctly once the
	// index returns.
	m, _ := settleMarket(t, func(u string) (float64, bool) { return 0, false }) // no index
	sym := "BTC-261231-40000-P"                                                 // the ITM put is the dangerous one
	if recs := m.Settle(context.Background(), &Settler{}, nil, nil); len(recs) != 0 {
		t.Fatalf("no index: nothing may settle: %+v", recs)
	}
	if m.IsSettled(sym) {
		t.Fatalf("the put must stay unsettled without an index (else its full strike 40000 is minted)")
	}

	// Now the index returns: the same sweep pass settles it correctly at
	// intrinsic, not the fabricated 0.
	m2, _ := settleMarket(t, func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 51000, true
		}
		return 0, false
	})
	recs := m2.Settle(context.Background(), &Settler{}, nil, nil)
	by := map[string]Settlement{}
	for _, r := range recs {
		by[r.Symbol] = r
	}
	want := map[string]float64{
		"BTC-261231-40000-P": 0,     // K 40k < index 51k → OTM put, 0
		"BTC-261231-40000-C": 11000, // 51k − 40k
		"BTC-261231-50000-C": 1000,  // 51k − 50k
		"BTC-261231-60000-P": 9000,  // 60k − 51k
	}
	for sym, v := range want {
		if by[sym].PerUnit != v {
			t.Errorf("%s perUnit %v want %v", sym, by[sym].PerUnit, v)
		}
	}
	if by["BTC-261231-40000-P"].PerUnit != 0 {
		t.Errorf("40k put at index 51k must be OTM 0, got %v (a fabricated index=0 would have been 40000)", by["BTC-261231-40000-P"].PerUnit)
	}
}

func TestSettle_ExoticPayoffErrorIsNotZero(t *testing.T) {
	m, mock := settleMarket(t, func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 55000, true
		}
		return 0, false
	})
	// The payoff fails to price: the exotic must be LEFT UNSETTLED (a
	// fabricated 0 would destroy the seller's ITM payoff) and the vanillas
	// still settle at their intrinsic.
	mock.err = context.DeadlineExceeded
	s := m.Settle(context.Background(), &Settler{}, nil, nil)
	if len(s) != 6 {
		t.Fatalf("vanillas must settle even when the exotic payoff fails; got %d", len(s))
	}
	for _, rec := range s {
		if rec.IsExotic {
			t.Fatalf("broken exotic must not settle at a fabricated value: %+v", rec)
		}
	}
	if m.IsSettled("BTC-261231-50000-BAR") {
		t.Error("broken exotic must stay unsettled")
	}
	// Recovery: the pricer returns and the next pass settles it.
	mock.err = nil
	s2 := m.Settle(context.Background(), &Settler{}, nil, nil)
	if len(s2) != 1 || !s2[0].IsExotic {
		t.Fatalf("recovery pass must settle only the exotic: %+v", s2)
	}
}
