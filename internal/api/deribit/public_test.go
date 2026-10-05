package deribit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

func testBook() *optmarket.Market {
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
	m.AddInstrument(optmarket.NewInstrument("BTC", "BTCUSDT", "USDT", 50000, options.Call, date))
	return m
}

func TestTicker_ATMCall(t *testing.T) {
	book := testBook()
	want, err := book.Mark("BTC-261231-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(book))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v2/public/ticker?instrument_name=BTC-31DEC26-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Result struct {
			InstrumentName string  `json:"instrument_name"`
			MarkPrice      float64 `json:"mark_price"`
			MarkIV         float64 `json:"mark_iv"`
			BidIV          float64 `json:"bid_iv"`
			AskIV          float64 `json:"ask_iv"`
			Greeks         struct {
				Rho float64 `json:"rho"`
			} `json:"greeks"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Result.InstrumentName != "BTC-31DEC26-50000-C" {
		t.Fatalf("symbol %q", body.Result.InstrumentName)
	}
	if body.Result.MarkPrice != want.Price {
		t.Fatalf("mark price %v want %v", body.Result.MarkPrice, want.Price)
	}
	if !(body.Result.BidIV < body.Result.MarkIV && body.Result.MarkIV < body.Result.AskIV) {
		t.Fatalf("iv bid/mark/ask %v %v %v", body.Result.BidIV, body.Result.MarkIV, body.Result.AskIV)
	}
	if body.Result.MarkIV != want.MarkIV*100 {
		t.Fatalf("mark iv %v want percent %v", body.Result.MarkIV, want.MarkIV*100)
	}
	if body.Result.Greeks.Rho == 0 {
		t.Fatal("deribit publishes rho")
	}
}
