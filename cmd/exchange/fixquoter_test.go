package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// recordingPricer is a fake Pricer that records every Price call and returns
// a fixed fair value. It lets the fixQuoter tests assert on WHAT the quoter
// sent (script, script_file, spot) and HOW OFTEN — the old put-call-parity
// bug made exactly TWO prices calls (a C leg and a P leg) per quote; the fix
// must make exactly ONE (the actual configured payoff).
type recordingPricer struct {
	price float64
	calls int64
	last  pricer.Params
	err   error
}

func (r *recordingPricer) Name() string { return "rec" }

func (r *recordingPricer) Price(_ context.Context, p pricer.Params) (pricer.Result, error) {
	atomic.AddInt64(&r.calls, 1)
	r.last = p
	if r.err != nil {
		return pricer.Result{}, r.err
	}
	return pricer.Result{Price: r.price, Raw: map[string]any{"price": r.price}}, nil
}

// bookAt builds a single-instrument book with the given best bid/ask.
func bookAt(t *testing.T, sym string, bid, ask string) *orderbook.OrderBook {
	t.Helper()
	b := orderbook.New([]string{sym})
	for _, o := range []*orderbook.Order{
		{ID: "b1", Instrument: sym, Side: orderbook.SideBuy, Price: decimal.MustParse(bid), Volume: decimal.MustParse("10")},
		{ID: "a1", Instrument: sym, Side: orderbook.SideSell, Price: decimal.MustParse(ask), Volume: decimal.MustParse("10")},
	} {
		if _, err := b.AddLimitOrder(o); err != nil {
			t.Fatalf("seeding book: %v", err)
		}
	}
	return b
}

func sharedModel() pricer.Params {
	return pricer.Params{Vol: 0.30, NumPaths: 65536, NumSteps: 252, Seed: 42, ExpiryYears: 1.0, Model: "bs"}
}

// TestFixQuoter_SingleCallAroundActualFairValue is the core regression test:
// the quote must be centered on the ACTUAL configured payoff's fair value
// (one pricer call, correct spot from the funding book, correct payoff), and
// the bid/offer must bracket it by exactly the configured half-spread.
func TestFixQuoter_SingleCallAroundActualFairValue(t *testing.T) {
	fair := 13.417598957079523 // recorded barrier golden (spot=100)
	px := &recordingPricer{price: fair}
	reg := pricer.NewRegistry()
	reg.Register(px)

	book := bookAt(t, "BTC-USD", "99.50", "101.50") // mid = 100.50
	q, err := newFixQuoter(reg, "rec", book,
		[]config.FIXExotic{{Engine: "BTC-EXOT", PayoffFile: "payoffs/barrier_down_out_call.aria"}},
		map[string]string{"BTC-EXOT": "BTC-USD"},
		10.0, // bps half-spread
		sharedModel())
	if err != nil {
		t.Fatalf("newFixQuoter: %v", err)
	}

	bid, offer, err := q.Quote(context.Background(), "BTC-EXOT", decimal.MustParse("1"))
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if n := atomic.LoadInt64(&px.calls); n != 1 {
		t.Fatalf("pricer called %d times, want exactly 1 (single actual payoff; the parity-pair bug made 2)", n)
	}
	p := px.last
	if p.Spot != 100.50 {
		t.Errorf("spot = %v, want the funding book mid 100.5", p.Spot)
	}
	if p.ScriptFile != "payoffs/barrier_down_out_call.aria" {
		t.Errorf("script_file = %q, want the configured payoff", p.ScriptFile)
	}
	if p.Script != "" {
		t.Errorf("script = %q, want empty (payoff_file mode)", p.Script)
	}
	if p.Strike != 0 || p.Side != "" {
		t.Errorf("strike=%v side=%q, want both cleared (no vanilla inference)", p.Strike, p.Side)
	}

	// bid = fair·(1−0.001), offer = fair·(1+0.001); midpoint = fair.
	mid := bid.Add(offer).Div(decimal.FromInt(2))
	wantFair := decimal.FromFloat(fair)
	if got := deltaRel(mid, wantFair); got > 1e-9 {
		t.Errorf("quote midpoint = %s, want fair %s (rel %g)", mid, wantFair, got)
	}
	if bid.Gt(offer) || !bid.Gt(decimal.Zero) {
		t.Errorf("uncrossed-positive violated: bid=%s offer=%s", bid, offer)
	}
	// The half-spread must be exactly w·fair each side.
	wantHalf := decimal.FromFloat(fair * 10.0 / 10000.0)
	if got := offer.Sub(mid); relErr(got, wantHalf) > 1e-9 {
		t.Errorf("offer−mid = %s, want %s", got, wantHalf)
	}
	if got := mid.Sub(bid); relErr(got, wantHalf) > 1e-9 {
		t.Errorf("mid−bid = %s, want %s", got, wantHalf)
	}
}

// TestFixQuoter_InlineScriptAndPerExoticOverrides pins the config plumbing:
// an inline Aria payoff is passed verbatim and the per-exotic vol/model
// overrides beat the shared defaults.
func TestFixQuoter_InlineScriptAndPerExoticOverrides(t *testing.T) {
	script := `contract Asian { let S = spot("X") one(KRW) |> scale(max(sumS/1 - 100.0, 0.0)) |> when(2026-01-01) }`
	px := &recordingPricer{price: 5.777010252130649}
	reg := pricer.NewRegistry()
	reg.Register(px)

	// bookAt seeds only ETH-USD; map the exotic to that funded underlying so
	// spot = ETH-USD book mid (1950).
	book := bookAt(t, "ETH-USD", "1900", "2000") // mid 1950
	q, err := newFixQuoter(reg, "rec", book,
		[]config.FIXExotic{{
			Engine: "ETH-EXOT",
			Payoff: script,
			Vol:    0.45,
			Model:  "dupire",
		}},
		map[string]string{"ETH-EXOT": "ETH-USD"},
		0, // default spread
		sharedModel())
	if err != nil {
		t.Fatalf("newFixQuoter: %v", err)
	}
	if _, _, err := q.Quote(context.Background(), "ETH-EXOT", decimal.FromInt(1)); err != nil {
		t.Fatalf("Quote on own book without spot_from: %v", err)
	}
	p := px.last
	if p.Script != script {
		t.Errorf("inline script not passed verbatim: %q", p.Script)
	}
	if p.ScriptFile != "" {
		t.Errorf("script_file = %q, want empty", p.ScriptFile)
	}
	if p.Vol != 0.45 {
		t.Errorf("vol = %v, want per-exotic override 0.45", p.Vol)
	}
	if p.Model != "dupire" {
		t.Errorf("model = %q, want per-exotic override dupire", p.Model)
	}
	if p.Spot != 1950 {
		t.Errorf("spot = %v, want own-book mid 1950", p.Spot)
	}
}

func TestFixQuoter_ConfigValidation(t *testing.T) {
	book := bookAt(t, "X", "1", "2")
	reg := pricer.NewRegistry()
	reg.Register(&recordingPricer{price: 1})

	bad := func(t *testing.T, ex []config.FIXExotic, why string) {
		t.Helper()
		if _, err := newFixQuoter(reg, "rec", book, ex, nil, 10, sharedModel()); err == nil {
			t.Errorf("newFixQuoter(%v) accepted, want error (%s)", ex, why)
		}
	}
	bad(t, []config.FIXExotic{{Engine: "A"}}, "neither payoff nor payoff_file")
	bad(t, []config.FIXExotic{{Engine: "A", Payoff: "x", PayoffFile: "y.aria"}}, "both payoff and payoff_file")
	bad(t, []config.FIXExotic{{Engine: "A", Payoff: "x"}, {Engine: "A", Payoff: "z"}}, "duplicate engine")
	bad(t, []config.FIXExotic{{Engine: "   ", Payoff: "x"}}, "blank engine")
}

// TestFixQuoter_Rejects exercises the error paths.
func TestFixQuoter_Rejects(t *testing.T) {
	reg := pricer.NewRegistry()
	reg.Register(&recordingPricer{price: 1})
	book := bookAt(t, "BTC-USD", "99.5", "101.5")
	mk := func(px *recordingPricer, spread float64) *fixQuoter {
		reg2 := pricer.NewRegistry()
		reg2.Register(px)
		q, err := newFixQuoter(reg2, "rec", book,
			[]config.FIXExotic{{Engine: "BTC-EXOT", PayoffFile: "payoffs/barrier_down_out_call.aria"}},
			map[string]string{"BTC-EXOT": "BTC-USD"},
			spread, sharedModel())
		if err != nil {
			t.Fatalf("newFixQuoter: %v", err)
		}
		return q
	}

	// Unknown instrument → error, no pricing attempted.
	if _, _, err := mk(&recordingPricer{price: 1}, 10).Quote(context.Background(), "NO-PE", decimal.FromInt(1)); err == nil {
		t.Error("Quote on unconfigured instrument: want error")
	}

	// Pricer failure propagates.
	if _, _, err := mk(&recordingPricer{price: 1, err: context.Canceled}, 10).Quote(context.Background(), "BTC-EXOT", decimal.FromInt(1)); err == nil {
		t.Error("Quote with failing pricer: want error")
	}

	// Non-positive fair value (payoff paying 0 everywhere at this spot) is a
	// quote error, never a free instrument.
	if _, _, err := mk(&recordingPricer{price: 0}, 10).Quote(context.Background(), "BTC-EXOT", decimal.FromInt(1)); err == nil {
		t.Error("Quote with zero fair value: want error (no free instruments)")
	}

	// Empty funding book → error.
	empty := orderbook.New([]string{"BTC-USD"})
	q, _ := newFixQuoter(reg, "rec", empty,
		[]config.FIXExotic{{Engine: "BTC-EXOT", PayoffFile: "p.aria"}},
		map[string]string{"BTC-EXOT": "BTC-USD"}, 10, sharedModel())
	if _, _, err := q.Quote(context.Background(), "BTC-EXOT", decimal.FromInt(1)); err == nil {
		t.Error("Quote with empty funding book: want error")
	}
}

// deltaRel = |a−b|/max(|b|,1) as a float relative measure.
func deltaRel(a, b decimal.Decimal) float64 {
	d := a.Sub(b)
	if d.Sign() < 0 {
		d = d.Neg()
	}
	den := b.Abs()
	if den.Lt(decimal.FromInt(1)) {
		den = decimal.FromInt(1)
	}
	return d.Div(den).Float64()
}

// relErr is |got−want|/|want| with a zero-guard.
func relErr(got, want decimal.Decimal) float64 {
	return deltaRel(got, want)
}
