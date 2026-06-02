package binance

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestExchangeInfo verifies the market-discovery document a stock client reads
// in loadMarkets(): all configured symbols, split into base/quote, with the
// precision filters CCXT parses.
func TestExchangeInfo(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.srv.URL + "/api/v3/exchangeInfo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var info exchangeInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Timezone != "UTC" || info.ServerTime == 0 {
		t.Errorf("bad envelope: tz=%q serverTime=%d", info.Timezone, info.ServerTime)
	}
	if len(info.Symbols) != 2 {
		t.Fatalf("got %d symbols, want 2", len(info.Symbols))
	}

	byID := map[string]symbolInfo{}
	for _, s := range info.Symbols {
		byID[s.Symbol] = s
	}
	btc, ok := byID["BTCUSDT"]
	if !ok {
		t.Fatalf("BTCUSDT missing; got %v", byID)
	}
	if btc.BaseAsset != "BTC" || btc.QuoteAsset != "USD" {
		t.Errorf("BTCUSDT base/quote = %q/%q, want BTC/USD", btc.BaseAsset, btc.QuoteAsset)
	}
	if btc.Status != "TRADING" || !btc.IsSpotTradingAllowed {
		t.Errorf("BTCUSDT not tradable: status=%q spot=%v", btc.Status, btc.IsSpotTradingAllowed)
	}
	// A client needs at least PRICE_FILTER + LOT_SIZE to derive precision.
	var hasPrice, hasLot bool
	for _, f := range btc.Filters {
		m, _ := f.(map[string]interface{})
		switch m["filterType"] {
		case "PRICE_FILTER":
			hasPrice = m["tickSize"] == "0.00000001"
		case "LOT_SIZE":
			hasLot = m["stepSize"] == "0.00000001"
		}
	}
	if !hasPrice || !hasLot {
		t.Errorf("missing/incorrect precision filters: price=%v lot=%v (%v)", hasPrice, hasLot, btc.Filters)
	}
}

// TestExchangeInfoSymbolFilter checks the ?symbol= narrowing and the unknown
// symbol error path.
func TestExchangeInfoSymbolFilter(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.srv.URL + "/api/v3/exchangeInfo?symbol=ETHUSDT")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var info exchangeInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(info.Symbols) != 1 || info.Symbols[0].Symbol != "ETHUSDT" {
		t.Fatalf("filter returned %v, want only ETHUSDT", info.Symbols)
	}

	bad, err := http.Get(h.srv.URL + "/api/v3/exchangeInfo?symbol=NOPE")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer bad.Body.Close()
	if bad.StatusCode == http.StatusOK {
		t.Errorf("unknown symbol should not return 200")
	}
}

func TestStepStr(t *testing.T) {
	cases := map[int]string{0: "1", 1: "0.1", 2: "0.01", 8: "0.00000001"}
	for prec, want := range cases {
		if got := stepStr(prec); got != want {
			t.Errorf("stepStr(%d) = %q, want %q", prec, got, want)
		}
	}
}
