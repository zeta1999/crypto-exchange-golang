package bybit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	resp, err := http.Get(srv.URL + "/v5/market/tickers?category=option&symbol=BTC-31DEC26-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		RetCode int `json:"retCode"`
		Result  struct {
			Category string `json:"category"`
			List     []struct {
				Symbol    string `json:"symbol"`
				MarkPrice string `json:"markPrice"`
				MarkIv    string `json:"markIv"`
				Bid1Iv    string `json:"bid1Iv"`
				Ask1Iv    string `json:"ask1Iv"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.RetCode != 0 || body.Result.Category != "option" || len(body.Result.List) != 1 {
		t.Fatalf("%+v", body)
	}
	row := body.Result.List[0]
	if row.Symbol != "BTC-31DEC26-50000-C" {
		t.Fatalf("symbol %q", row.Symbol)
	}
	if row.MarkPrice != strconv.FormatFloat(want.Price, 'f', 8, 64) {
		t.Fatalf("mark %s want %v", row.MarkPrice, want.Price)
	}
	bid, mark, ask := parseNum(t, row.Bid1Iv), parseNum(t, row.MarkIv), parseNum(t, row.Ask1Iv)
	if !(bid < mark && mark < ask) {
		t.Fatalf("iv bid/mark/ask %v %v %v", bid, mark, ask)
	}
}

func parseNum(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
