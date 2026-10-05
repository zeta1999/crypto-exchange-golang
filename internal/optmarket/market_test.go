package optmarket

import (
	"math"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// fixedClock + fixed index make the whole surface deterministic, so its output
// can be captured as a recorded non-regression fixture (CR-9). The clock is
// 2026-06-04 08:00 UTC; the chain expires 2026-12-31 08:00 UTC (T≈0.575y).
func testMarket(t *testing.T) *Market {
	t.Helper()
	now := time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC)
	index := func(underlying string) (float64, bool) {
		if underlying == "BTCUSDT" {
			return 50000, true
		}
		return 0, false
	}
	m := NewMarket(func() time.Time { return now }, index,
		0.03 /*rate*/, 0.02 /*ivSpread*/, 0.02 /*bookHalf*/, 0.5 /*priceCap*/)
	m.SetSurface("BTCUSDT", VolSurface{ATMVol: 0.65, Skew: -0.05, Smile: 0.10})
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	for _, strike := range []float64{40000, 50000, 60000} {
		m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", strike, options.Call, date))
		m.AddInstrument(NewInstrument("BTC", "BTCUSDT", "USDT", strike, options.Put, date))
	}
	return m
}

func TestMarket_MarkSanity(t *testing.T) {
	m := testMarket(t)
	// ATM call (strike=index=50000): delta in a sane band, positive gamma/vega.
	md, err := m.Mark("BTC-261231-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	if md.Delta < 0.45 || md.Delta > 0.75 {
		t.Errorf("ATM call delta %.4f out of sane band", md.Delta)
	}
	if md.Gamma <= 0 || md.Vega <= 0 {
		t.Errorf("ATM call gamma/vega must be positive: %v / %v", md.Gamma, md.Vega)
	}
	if md.Rho == 0 {
		t.Error("rho is on the book; the Binance adapter is what omits it")
	}
	mp, _ := m.Mark("BTC-261231-50000-P")
	if mp.Delta >= 0 {
		t.Errorf("put delta must be negative, got %v", mp.Delta)
	}
	if md.BidIV >= md.MarkIV || md.AskIV <= md.MarkIV {
		t.Errorf("bid/ask IV must straddle mark IV")
	}
}

func TestMarket_PutCallParity(t *testing.T) {
	m := testMarket(t)
	// Same strike → same surface vol → BS call/put satisfy parity exactly.
	c := mark(t, m, "BTC-261231-50000-C").Price
	p := mark(t, m, "BTC-261231-50000-P").Price
	idx := 50000.0
	in, _ := ParseSymbol("BTC-261231-50000-C", "USDT")
	tt := in.TimeToExpiry(time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC))
	want := idx - 50000*math.Exp(-0.03*tt)
	if math.Abs((c-p)-want) > 1e-3 {
		t.Errorf("parity: C-P=%.4f want %.4f", c-p, want)
	}
}

func TestMarket_DepthWellFormed(t *testing.T) {
	m := testMarket(t)
	d, err := m.Depth("BTC-261231-50000-C", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Asks) != 5 {
		t.Fatalf("want 5 ask levels, got %d", len(d.Asks))
	}
	for i := 1; i < len(d.Asks); i++ {
		if d.Asks[i].Price <= d.Asks[i-1].Price {
			t.Error("asks must strictly increase")
		}
	}
	for _, b := range d.Bids {
		if b.Price <= 0 {
			t.Error("bid price must be positive")
		}
	}
}

func TestMarket_ExpiredAndUnknown(t *testing.T) {
	m := testMarket(t)
	if _, err := m.Mark("BTC-261231-99999-C"); err == nil {
		t.Error("unknown symbol must error")
	}
	// An instrument whose underlying has no index is skipped by MarkAll.
	if got := len(m.Marks()); got != 6 {
		t.Errorf("want 6 priced instruments, got %d", got)
	}
}

func TestMarket_ZeroDividendMatchesSpotCarry(t *testing.T) {
	m := testMarket(t)
	md := mark(t, m, "BTC-261231-50000-C")
	in, err := ParseSymbol("BTC-261231-50000-C", "USDT")
	if err != nil {
		t.Fatal(err)
	}
	tt := in.TimeToExpiry(time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC))
	fwd := CarryForward(50000, 0.03, 0, tt)
	vol := (VolSurface{ATMVol: 0.65, Skew: -0.05, Smile: 0.10}).Vol(50000, fwd, tt)
	want := options.Compute(options.Call, 50000, 50000, tt, 0.03, vol)
	if math.Abs(md.Price-want.Price) > 1e-9 || math.Abs(md.Delta-want.Delta) > 1e-12 {
		t.Fatalf("q=0 mark drifted: %+v want price %.8f", md, want.Price)
	}
}

func mark(t *testing.T, m *Market, sym string) Mark {
	t.Helper()
	md, err := m.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}
	return md
}
