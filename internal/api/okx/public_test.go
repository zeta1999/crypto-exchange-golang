package okx

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

func TestSummary_ATMCall(t *testing.T) {
	book := testBook()
	want, err := book.Mark("BTC-261231-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(book))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v5/public/opt-summary?instId=BTC-USD-261231-50000-C")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Code string `json:"code"`
		Data []struct {
			InstID  string `json:"instId"`
			MarkPx  string `json:"markPx"`
			MarkVol string `json:"markVol"`
			BidVol  string `json:"bidVol"`
			AskVol  string `json:"askVol"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "0" || len(body.Data) != 1 {
		t.Fatalf("%+v", body)
	}
	row := body.Data[0]
	if row.InstID != "BTC-USD-261231-50000-C" {
		t.Fatalf("symbol %q", row.InstID)
	}
	if row.MarkPx != strconv.FormatFloat(want.Price, 'f', 8, 64) {
		t.Fatalf("mark %s want %v", row.MarkPx, want.Price)
	}
	bid, mark, ask := parseNum(t, row.BidVol), parseNum(t, row.MarkVol), parseNum(t, row.AskVol)
	if !(bid < mark && mark < ask) {
		t.Fatalf("vol bid/mark/ask %v %v %v", bid, mark, ask)
	}
	if row.MarkVol != strconv.FormatFloat(want.MarkIV, 'f', 8, 64) {
		t.Fatalf("mark vol %s want %v", row.MarkVol, want.MarkIV)
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
