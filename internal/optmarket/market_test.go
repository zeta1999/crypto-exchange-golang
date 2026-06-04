package optmarket

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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
	delta := mustF(t, md.Delta)
	if delta < 0.45 || delta > 0.75 {
		t.Errorf("ATM call delta %.4f out of sane band", delta)
	}
	if mustF(t, md.Gamma) <= 0 || mustF(t, md.Vega) <= 0 {
		t.Errorf("ATM call gamma/vega must be positive: %s / %s", md.Gamma, md.Vega)
	}
	// put delta is negative
	mp, _ := m.Mark("BTC-261231-50000-P")
	if mustF(t, mp.Delta) >= 0 {
		t.Errorf("put delta must be negative, got %s", mp.Delta)
	}
	// bid/ask IV straddle mark IV; no rho field is emitted (Binance EAPI shape)
	if mustF(t, md.BidIV) >= mustF(t, md.MarkIV) || mustF(t, md.AskIV) <= mustF(t, md.MarkIV) {
		t.Errorf("bid/ask IV must straddle mark IV")
	}
	raw, _ := json.Marshal(md)
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)
	if _, ok := asMap["rho"]; ok {
		t.Error("EAPI mark must NOT carry rho")
	}
}

func TestMarket_PutCallParity(t *testing.T) {
	m := testMarket(t)
	// Same strike → same surface vol → BS call/put satisfy parity exactly.
	c := mustF(t, mark(t, m, "BTC-261231-50000-C").MarkPrice)
	p := mustF(t, mark(t, m, "BTC-261231-50000-P").MarkPrice)
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
	// asks strictly increasing, bids strictly decreasing, all positive
	for i := 1; i < len(d.Asks); i++ {
		if mustF(t, d.Asks[i][0]) <= mustF(t, d.Asks[i-1][0]) {
			t.Error("asks must strictly increase")
		}
	}
	for _, b := range d.Bids {
		if mustF(t, b[0]) <= 0 {
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
	if got := len(m.MarkAll()); got != 6 {
		t.Errorf("want 6 priced instruments, got %d", got)
	}
}

// Recorded golden: the full EAPI surface (exchangeInfo + all marks + a depth +
// index) captured as a deterministic non-regression fixture (CR-9). Refresh with
// `UPDATE_GOLDEN=1 go test ./internal/optmarket/...`.
func TestMarket_GoldenSnapshot(t *testing.T) {
	m := testMarket(t)
	depth, _ := m.Depth("BTC-261231-50000-C", 3)
	idx, _ := m.Index("BTCUSDT")
	snap := map[string]any{
		"exchangeInfo": m.ExchangeInfo(),
		"mark":         m.MarkAll(),
		"depth":        depth,
		"index":        idx,
	}
	got, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "..", "testdata", "optmarket", "eapi_snapshot.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated golden %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch — run UPDATE_GOLDEN=1 to refresh.\n--- got ---\n%s", got)
	}
}

func mark(t *testing.T, m *Market, sym string) MarkData {
	t.Helper()
	md, err := m.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func mustF(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := jsonNumber(s, &f); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return f
}

func jsonNumber(s string, out *float64) (int, error) {
	n, err := json.Number(s).Float64()
	if err != nil {
		return 0, err
	}
	*out = n
	return 1, nil
}
