package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// optTestMarket is a deterministic options surface for the EAPI handler tests:
// a fixed clock + fixed BTCUSDT index, a 3-strike chain expiring 2026-12-31.
func optTestMarket() *optmarket.Market {
	now := time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC)
	index := func(u string) (float64, bool) {
		if u == "BTCUSDT" {
			return 50000, true
		}
		return 0, false
	}
	m := optmarket.NewMarket(func() time.Time { return now }, index, 0.03, 0.02, 0.02, 0.5)
	m.SetSurface("BTCUSDT", optmarket.VolSurface{ATMVol: 0.65, Skew: -0.05, Smile: 0.10})
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	for _, k := range []float64{40000, 50000, 60000} {
		m.AddInstrument(optmarket.NewInstrument("BTC", "BTCUSDT", "USDT", k, options.Call, date))
		m.AddInstrument(optmarket.NewInstrument("BTC", "BTCUSDT", "USDT", k, options.Put, date))
	}
	return m
}

func TestEAPI_ExchangeInfo(t *testing.T) {
	h := newHarness(t, WithOptionsMarket(optTestMarket()))
	resp := h.get(t, "/eapi/v1/exchangeInfo")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var info ExchangeInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if len(info.OptionSymbols) != 6 {
		t.Fatalf("want 6 option symbols, got %d", len(info.OptionSymbols))
	}
	if info.Timezone != "UTC" {
		t.Errorf("tz=%q", info.Timezone)
	}
	got := map[string]bool{}
	for _, s := range info.OptionSymbols {
		got[s.Symbol] = true
		if s.Underlying != "BTCUSDT" || s.QuoteAsset != "USDT" {
			t.Errorf("bad spec for %s: %+v", s.Symbol, s)
		}
	}
	if !got["BTC-261231-50000-C"] || !got["BTC-261231-40000-P"] {
		t.Errorf("missing expected symbols: %v", got)
	}
}

func TestEAPI_MarkSingleAndAll(t *testing.T) {
	h := newHarness(t, WithOptionsMarket(optTestMarket()))

	// single symbol → 1-element array
	resp := h.get(t, "/eapi/v1/mark?symbol=BTC-261231-50000-C")
	defer resp.Body.Close()
	var one []MarkData
	if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Symbol != "BTC-261231-50000-C" {
		t.Fatalf("want 1 mark for the symbol, got %+v", one)
	}
	if one[0].MarkPrice == "" || one[0].MarkIV == "" || one[0].Delta == "" || one[0].Gamma == "" || one[0].Vega == "" {
		t.Errorf("mark missing fields: %+v", one[0])
	}

	// raw JSON must NOT carry rho (Binance EAPI shape)
	resp2 := h.get(t, "/eapi/v1/mark?symbol=BTC-261231-50000-C")
	defer resp2.Body.Close()
	var rawArr []map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&rawArr)
	if _, ok := rawArr[0]["rho"]; ok {
		t.Error("EAPI mark must not include rho")
	}

	// all marks
	respAll := h.get(t, "/eapi/v1/mark")
	defer respAll.Body.Close()
	var all []MarkData
	if err := json.NewDecoder(respAll.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("want 6 marks, got %d", len(all))
	}
}

func TestEAPI_Depth(t *testing.T) {
	h := newHarness(t, WithOptionsMarket(optTestMarket()))
	resp := h.get(t, "/eapi/v1/depth?symbol=BTC-261231-50000-C&limit=5")
	defer resp.Body.Close()
	var d Depth
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if len(d.Asks) != 5 || len(d.Bids) == 0 {
		t.Fatalf("bad depth: %d bids / %d asks", len(d.Bids), len(d.Asks))
	}
	if d.T == 0 {
		t.Error("depth missing time")
	}
}

func TestEAPI_Index(t *testing.T) {
	h := newHarness(t, WithOptionsMarket(optTestMarket()))
	resp := h.get(t, "/eapi/v1/index?underlying=BTCUSDT")
	defer resp.Body.Close()
	var idx IndexData
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if idx.IndexPrice != "50000.00000000" {
		t.Errorf("indexPrice=%q", idx.IndexPrice)
	}
}

func TestEAPI_UnknownSymbolErrors(t *testing.T) {
	h := newHarness(t, WithOptionsMarket(optTestMarket()))
	for _, path := range []string{
		"/eapi/v1/mark?symbol=BTC-261231-99999-C",
		"/eapi/v1/depth?symbol=BTC-261231-99999-C",
		"/eapi/v1/index?underlying=DOGEUSDT",
	} {
		resp := h.get(t, path)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: expected non-200 for unknown, got 200", path)
		}
		resp.Body.Close()
	}
}

// eapiExoticMarket is optTestMarket with one flow exotic attached, so the
// exotic endpoint can be exercised without the saga binary.
func eapiExoticMarket() (*optmarket.Market, *mockEapiPricer) {
	m := optTestMarket()
	px := &mockEapiPricer{price: 1234.56}
	reg := pricer.NewRegistry()
	reg.Register(px)
	if err := m.AttachPricer(reg, "mock", pricer.Params{Vol: 0.30, Rate: 0.03}, 5*time.Second, 0, 0.01); err != nil {
		panic(err)
	}
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	ex := optmarket.NewExoticInstrument("BTC", "BTCUSDT", "USDT", 50000, options.Call, date, "BTC-261231-50000-BAR")
	ex.PayoffFile = "payoffs/barrier_down_out_call.aria"
	if err := m.AddExotic(ex); err != nil {
		panic(err)
	}
	return m, px
}

func TestEAPI_MarkExotic(t *testing.T) {
	m, px := eapiExoticMarket()
	h := newHarness(t, WithOptionsMarket(m))

	// The exotic mark endpoint: one element, executable bid/ask bracketing the
	// MC fair value, IV/greeks zero, provenance present.
	resp := h.get(t, "/eapi/v1/mark/exotic?symbol=BTC-261231-50000-BAR")
	defer resp.Body.Close()
	var out []ExoticMarkData
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 exotic mark, got %d", len(out))
	}
	e := out[0]
	if e.Symbol != "BTC-261231-50000-BAR" || e.Pricer != "mock" {
		t.Errorf("bad exotic mark provenance: %+v", e)
	}
	if e.MarkIV != "0.00000000" || e.BidIV != "0.00000000" || e.AskIV != "0.00000000" {
		t.Errorf("exotic IV must be 0, got %+v", e)
	}
	if px.calls != 1 {
		t.Errorf("exotic must be priced once per TTL, got %d calls", px.calls)
	}

	// /eapi/v1/mark stays vanilla-only: it rejects the exotic symbol and the
	// all-marks list contains only the 6 vanilla contracts.
	if r := h.get(t, "/eapi/v1/mark?symbol=BTC-261231-50000-BAR"); r.StatusCode == http.StatusOK {
		t.Error("vanilla mark endpoint must reject an exotic symbol")
		r.Body.Close()
	} else {
		r.Body.Close()
	}
	rAll := h.get(t, "/eapi/v1/mark")
	defer rAll.Body.Close()
	var all []MarkData
	_ = json.NewDecoder(rAll.Body).Decode(&all)
	if len(all) != 6 {
		t.Errorf("/eapi/v1/mark must be vanilla-only (6), got %d", len(all))
	}

	// exchangeInfo lists the exotic contract too.
	rInfo := h.get(t, "/eapi/v1/exchangeInfo")
	defer rInfo.Body.Close()
	var info ExchangeInfo
	_ = json.NewDecoder(rInfo.Body).Decode(&info)
	found := false
	for _, s := range info.OptionSymbols {
		if s.Symbol == "BTC-261231-50000-BAR" {
			found = true
		}
	}
	if !found {
		t.Error("exotic contract missing from exchangeInfo")
	}

	// depth on the exotic is the executable quote itself (1 level per side,
	// uncrossed).
	rd := h.get(t, "/eapi/v1/depth?symbol=BTC-261231-50000-BAR")
	defer rd.Body.Close()
	var d Depth
	_ = json.NewDecoder(rd.Body).Decode(&d)
	if len(d.Bids) != 1 || len(d.Asks) != 1 {
		t.Errorf("exotic depth must be the quote itself (1/1), got %d/%d", len(d.Bids), len(d.Asks))
	}
	bid, _ := strconv.ParseFloat(d.Bids[0][0], 64)
	ask, _ := strconv.ParseFloat(d.Asks[0][0], 64)
	if bid >= ask {
		t.Errorf("exotic book crossed: bid=%v ask=%v", bid, ask)
	}
}

// mockEapiPricer is a recording in-memory Pricer for the EAPI exotic endpoint.
type mockEapiPricer struct {
	price float64
	calls int
}

func (m *mockEapiPricer) Name() string { return "mock" }
func (m *mockEapiPricer) Price(_ context.Context, _ pricer.Params) (pricer.Result, error) {
	m.calls++
	return pricer.Result{Price: m.price}, nil
}

// Recorded golden: the full EAPI surface captured as a deterministic fixture.
// q=0 must reproduce it byte for byte. Refresh with UPDATE_GOLDEN=1.
func TestEAPI_GoldenSnapshot(t *testing.T) {
	m := optTestMarket()
	depth, err := m.Depth("BTC-261231-50000-C", 3)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := m.Index("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	snap := map[string]any{
		"exchangeInfo": ProjectExchangeInfo(m.Contracts(), idx.TimeMs),
		"mark":         ProjectMarks(m.Marks()),
		"depth":        ProjectDepth(depth),
		"index":        ProjectIndex(idx),
	}
	got, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "..", "..", "testdata", "optmarket", "eapi_snapshot.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("EAPI golden mismatch (q=0 must match the recorded book):\n%s", got)
	}
}

// Without WithOptionsMarket, the /eapi routes are not registered.
func TestEAPI_DisabledByDefault(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/eapi/v1/exchangeInfo")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("eapi should be 404 when no options market is wired, got %d", resp.StatusCode)
	}
}
