package binance

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
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
	var info optmarket.ExchangeInfo
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
	var one []optmarket.MarkData
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
	var all []optmarket.MarkData
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
	var d optmarket.Depth
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
	var idx optmarket.IndexData
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

// Without WithOptionsMarket, the /eapi routes are not registered.
func TestEAPI_DisabledByDefault(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/eapi/v1/exchangeInfo")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("eapi should be 404 when no options market is wired, got %d", resp.StatusCode)
	}
}
