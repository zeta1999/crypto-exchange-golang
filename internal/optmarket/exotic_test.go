package optmarket

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// mockPricer is an in-memory Pricer for exercising the exotic pricing path
// without the saga binary: it records every call and returns a fixed price
// (or error). The tests assert the contract: ONE price call per exotic per
// TTL, the ACTUAL payoff params passed through, fair value bracketed, no IV
// or greeks invented.
type mockPricer struct {
	price float64
	err   error
	calls int
	last  pricer.Params
}

func (m *mockPricer) Name() string { return "mock" }
func (m *mockPricer) Price(_ context.Context, p pricer.Params) (pricer.Result, error) {
	m.calls++
	m.last = p
	if m.err != nil {
		return pricer.Result{}, m.err
	}
	return pricer.Result{Price: m.price}, nil
}

type exoticClock struct{ now time.Time }

func (c *exoticClock) Now() time.Time          { return c.now }
func (c *exoticClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func exoticTestMarket(px *mockPricer) (*Market, *pricer.Registry, *exoticClock) {
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	clk := &exoticClock{now: now}
	index := func(underlying string) (float64, bool) {
		if underlying == "BTCUSDT" {
			return 100, true
		}
		return 0, false
	}
	m := NewMarket(clk.Now, index, 0.03, 0.02, 0.02, 0.5)
	m.SetSurface("BTCUSDT", VolSurface{ATMVol: 0.30})
	// One vanilla for mixed-marks coverage.
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", 100, options.Call, date))
	reg := pricer.NewRegistry()
	reg.Register(px)
	if err := m.AttachPricer(reg, "mock", pricer.Params{Vol: 0.30, Rate: 0.03, NumPaths: 4096, NumSteps: 252, Seed: 7}, 5*time.Second, 0, 0); err != nil {
		panic(err)
	}
	return m, reg, clk
}

func addBarrierExotic(t *testing.T, m *Market) {
	t.Helper()
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	ex := NewExoticInstrument("BTC", "BTCUSDT", "USDT", 100, options.Call, date, "BTC-261231-100-BAR")
	ex.PayoffFile = "payoffs/barrier_down_out_call.aria"
	if err := m.AddExotic(ex); err != nil {
		t.Fatal(err)
	}
}

func TestExotic_SingleCallBracketedFairValue(t *testing.T) {
	px := &mockPricer{price: 12.5}
	m, _, _ := exoticTestMarket(px)
	addBarrierExotic(t, m)

	mk, err := m.Mark("BTC-261231-100-BAR")
	if err != nil {
		t.Fatal(err)
	}
	// One price call — the payoff is priced directly, not via two vanilla
	// legs or a parity fabrication.
	if px.calls != 1 {
		t.Fatalf("want exactly 1 pricer call, got %d", px.calls)
	}
	// The ACTUAL configured payoff + market state reached the pricer.
	if px.last.ScriptFile != "payoffs/barrier_down_out_call.aria" || px.last.Script != "" {
		t.Errorf("wrong payoff passed to pricer: %+v", px.last)
	}
	if px.last.Spot != 100 || px.last.Vol != 0.30 || px.last.Rate != 0.03 {
		t.Errorf("wrong market state passed to pricer: %+v", px.last)
	}
	// Strike/Side are cleared: the script IS the instrument.
	if px.last.Strike != 0 || px.last.Side != "" {
		t.Errorf("Strike/Side must be cleared for an exotic, got %+v", px.last)
	}
	// The quote brackets the SINGLE fair value by the half-spread
	// (bookHalf=0.02 here, no bps override).
	if mk.Price != 12.5 {
		t.Errorf("mark=%v want fair 12.5", mk.Price)
	}
	wantBid, wantAsk := 12.5*(1-0.02), 12.5*(1+0.02)
	if math.Abs(mk.Bid-wantBid) > 1e-12 || math.Abs(mk.Ask-wantAsk) > 1e-12 {
		t.Errorf("bid/ask=%v/%v want %v/%v", mk.Bid, mk.Ask, wantBid, wantAsk)
	}
	if mk.Bid >= mk.Price || mk.Ask <= mk.Price {
		t.Errorf("quote must bracket the fair value: %+v", mk)
	}
	// Not a BS instrument: no implied vol, no greeks — provenance instead.
	if mk.BidIV != 0 || mk.AskIV != 0 || mk.MarkIV != 0 || mk.Delta != 0 || mk.Gamma != 0 || mk.Vega != 0 || mk.Theta != 0 || mk.Rho != 0 {
		t.Errorf("exotic mark must not carry IV/greeks: %+v", mk)
	}
	if mk.Pricer != "mock" {
		t.Errorf("pricer provenance missing: %+v", mk)
	}
}

func TestExotic_CacheTTL(t *testing.T) {
	px := &mockPricer{price: 12.5}
	m, _, clk := exoticTestMarket(px)
	addBarrierExotic(t, m)
	// 10 mark requests inside the 5s TTL → still ONE pricer call.
	for i := 0; i < 10; i++ {
		if _, err := m.Mark("BTC-261231-100-BAR"); err != nil {
			t.Fatal(err)
		}
	}
	if px.calls != 1 {
		t.Fatalf("cache missed: %d pricer calls within the TTL", px.calls)
	}
	clk.advance(6 * time.Second)
	if _, err := m.Mark("BTC-261231-100-BAR"); err != nil {
		t.Fatal(err)
	}
	if px.calls != 2 {
		t.Fatalf("expected a re-price after the TTL, calls=%d", px.calls)
	}
}

func TestExotic_PerExoticOverridesAndFallbackVol(t *testing.T) {
	px := &mockPricer{price: 12.5}
	m, _, _ := exoticTestMarket(px)
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	// Per-exotic vol + model overrides beat the shared model.
	ex1 := NewExoticInstrument("BTC", "BTCUSDT", "USDT", 100, options.Call, date, "BTC-OVR-BAR")
	ex1.Payoff = "contract X { ... }"
	ex1.Vol = 0.42
	ex1.Model = "dupire"
	if err := m.AddExotic(ex1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Mark("BTC-OVR-BAR"); err != nil {
		t.Fatal(err)
	}
	if px.last.Vol != 0.42 || px.last.Model != "dupire" {
		t.Errorf("overrides not applied: vol=%v model=%v", px.last.Vol, px.last.Model)
	}
	if px.last.Script != "contract X { ... }" {
		t.Errorf("inline payoff not passed verbatim: %q", px.last.Script)
	}

	// Vol==0 with shared vol==0 falls back to the underlying surface at the
	// strike (ATM with flat 0.30 → 0.30).
	px2 := &mockPricer{price: 5}
	reg := pricer.NewRegistry()
	reg.Register(px2)
	m2 := NewMarket(time.Now, func(u string) (float64, bool) { return 100, true }, 0.03, 0.02, 0.02, 0.5)
	m2.SetSurface("BTCUSDT", VolSurface{ATMVol: 0.30})
	if err := m2.AttachPricer(reg, "mock", pricer.Params{}, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	ex2 := NewExoticInstrument("BTC", "BTCUSDT", "USDT", 100, options.Call, date, "BTC-FBV-BAR")
	ex2.Payoff = "contract X { ... }"
	if err := m2.AddExotic(ex2); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Mark("BTC-FBV-BAR"); err != nil {
		t.Fatal(err)
	}
	if px2.last.Vol <= 0 {
		t.Errorf("fallback vol must be > 0 (surface at strike), got %v", px2.last.Vol)
	}
}

func TestExotic_RiskLimitAndNegativeCache(t *testing.T) {
	px := &mockPricer{price: 1000000}
	m, _, _ := exoticTestMarket(px)
	// Risk limit: fair 1e6 > limit 1000 → not quoted, rejected.
	if err := m.AttachPricer(m.reg, "mock", pricer.Params{Vol: 0.3}, 5*time.Second, 1000, 0); err != nil {
		t.Fatal(err)
	}
	addBarrierExotic(t, m)
	if _, err := m.Mark("BTC-261231-100-BAR"); err == nil {
		t.Fatal("fair value above the risk limit must be rejected")
	}
	// Negative cache: repeated marks do not re-run the engine.
	for i := 0; i < 5; i++ {
		if _, err := m.Mark("BTC-261231-100-BAR"); err == nil {
			t.Fatalf("still rejected (expected), call %d", i)
		}
	}
	if px.calls != 1 {
		t.Fatalf("risk-limit rejection must be negatively cached: %d calls", px.calls)
	}
	// Marks() skips the rejected exotic, keeps the vanilla.
	mks := m.Marks()
	if len(mks) != 1 || mks[0].Pricer != "" {
		t.Errorf("Marks must contain only the cleanly-priced vanilla, got %v", mks)
	}
}

func TestExotic_NoFreeInstrumentsAndErrors(t *testing.T) {
	px := &mockPricer{price: 0}
	m, _, _ := exoticTestMarket(px)
	addBarrierExotic(t, m)
	if _, err := m.Mark("BTC-261231-100-BAR"); err == nil {
		t.Fatal("zero fair value must be an error, never a free instrument")
	}

	px.err = context.DeadlineExceeded
	for i := 0; i < 3; i++ {
		if _, err := m.Mark("BTC-261231-100-BAR"); err == nil {
			t.Fatal("pricer failure must surface as an error")
		}
	}
	if px.calls != 1 {
		t.Errorf("pricer failure must be negatively cached: %d calls", px.calls)
	}
}

func TestExotic_HalfSpreadOverride(t *testing.T) {
	px := &mockPricer{price: 12.5}
	m, _, _ := exoticTestMarket(px)
	// Re-attach with a bps-style half-spread fraction of 0.01 (10 bps).
	if err := m.AttachPricer(m.reg, "mock", pricer.Params{Vol: 0.3}, 0, 0, 0.01); err != nil {
		t.Fatal(err)
	}
	addBarrierExotic(t, m)
	mk, err := m.Mark("BTC-261231-100-BAR")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(mk.Bid-12.5*0.99) > 1e-12 || math.Abs(mk.Ask-12.5*1.01) > 1e-12 {
		t.Errorf("bid/ask=%v/%v want 12.375/12.625 (10bps bracket)", mk.Bid, mk.Ask)
	}
}

func TestExotic_DepthAndContracts(t *testing.T) {
	px := &mockPricer{price: 12.5}
	m, _, _ := exoticTestMarket(px)
	addBarrierExotic(t, m)
	d, err := m.Depth("BTC-261231-100-BAR", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Bids) != 1 || len(d.Asks) != 1 {
		t.Fatalf("exotic depth is the executable quote itself (1 per side), got %d/%d", len(d.Bids), len(d.Asks))
	}
	if d.Bids[0].Price >= d.Asks[0].Price {
		t.Errorf("exotic book crossed: %+v", d)
	}
	// Vanilla ladder still works side by side.
	vd, err := m.Depth("BTC-261231-100-C", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(vd.Asks) != 3 {
		t.Errorf("vanilla depth levels lost: %d", len(vd.Asks))
	}
	// Contracts lists both.
	cs := m.Contracts()
	if len(cs) != 2 {
		t.Fatalf("want 2 contracts (1 vanilla + 1 exotic), got %d", len(cs))
	}
	found := false
	for _, c := range cs {
		if c.Symbol == "BTC-261231-100-BAR" {
			found = true
		}
	}
	if !found {
		t.Errorf("exotic missing from contract list: %+v", cs)
	}
}

func TestAddExotic_Validation(t *testing.T) {
	px := &mockPricer{}
	m, _, _ := exoticTestMarket(px)
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	mk := func(mutate func(*Exotic)) error {
		e := NewExoticInstrument("BTC", "BTCUSDT", "USDT", 100, options.Call, date, "BTC-TEST-X")
		mutate(&e)
		return m.AddExotic(e)
	}
	cases := []struct {
		name   string
		mutate func(*Exotic)
	}{
		{"empty payoff", func(e *Exotic) {}},
		{"both payoffs", func(e *Exotic) { e.Payoff = "x"; e.PayoffFile = "y.aria" }},
		{"side token C", func(e *Exotic) { e.Payoff = "x"; e.VenueSymbol = "BTC-261231-100-C" }},
		{"side token P", func(e *Exotic) { e.Payoff = "x"; e.VenueSymbol = "BTC-261231-100-P" }},
		{"trailing dash", func(e *Exotic) { e.Payoff = "x"; e.VenueSymbol = "BTC-261231-100-" }},
		{"no underlying", func(e *Exotic) { e.Payoff = "x"; e.Underlying = "" }},
		{"nonpositive strike", func(e *Exotic) { e.Payoff = "x"; e.Strike = 0 }},
	}
	for _, c := range cases {
		if err := mk(c.mutate); err == nil {
			t.Errorf("%s: AddExotic must reject", c.name)
		}
	}
	// The good one passes, and is idempotent.
	if err := mk(func(e *Exotic) { e.Payoff = "x" }); err != nil {
		t.Errorf("valid exotic rejected: %v", err)
	}
	if err := mk(func(e *Exotic) { e.Payoff = "x" }); err != nil {
		t.Errorf("duplicate AddExotic must be idempotent: %v", err)
	}
	if len(m.ExoticSymbols()) != 1 {
		t.Errorf("want 1 registered exotic, got %v", m.ExoticSymbols())
	}
}

func TestAttachPricer_RejectsUnknownBackend(t *testing.T) {
	idx := func(u string) (float64, bool) { return 100, true }
	m := NewMarket(time.Now, idx, 0.03, 0.02, 0.02, 0.5)
	reg := pricer.NewRegistry()
	if err := m.AttachPricer(reg, "saga", pricer.Params{}, 0, 0, 0); err == nil {
		t.Error("AttachPricer must reject an unregistered backend name")
	}
	if m.HasPricer() {
		t.Error("HasPricer must be false after a failed attach")
	}
}
