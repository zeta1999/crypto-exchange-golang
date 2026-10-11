package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// optOrderHarness is a SIGNED harness with a seeded balance ledger and an
// options market whose option symbols are registered on the engine book (as
// main.go does), so option order entry is exercised end to end.
type optOrderHarness struct {
	*testHarness
	bsrv   *Server
	ledger *account.Ledger
	market *optmarket.Market
	nowFn  func() time.Time // the clock the auth timestamp must be near
}

// newOptOrderHarness builds a signed harness (its own fixed 2033 wall clock)
// with a seeded balance ledger and an options market whose symbols are
// registered on the engine book (as main.go does).
func newOptOrderHarness(t *testing.T, m *optmarket.Market, seed map[string]string) *optOrderHarness {
	// Fixed clock = optTestMarket's now (2026-06-04 08:00 UTC): the chain
	// hasn't expired (2026-12-31), so the place-time expiry check is clear,
	// and signed-request timestamps (off this same clock) are inside the
	// 5s recvWindow. Production shares one wall clock between edge and book.
	nowMs := int64(1780560000000)
	clk := func() time.Time { return time.UnixMilli(nowMs).UTC() }
	return newOptOrderHarnessClock(t, m, seed, clk)
}

// newOptOrderHarnessClock is newOptOrderHarness with an external clock, so the
// TEST can drive one shared clock across the market AND the edge — production
// has a single wall clock driving both. SettleSweep uses it to place before
// expiry, advance to expiry, and settle.
func newOptOrderHarnessClock(t *testing.T, m *optmarket.Market, seed map[string]string, clock func() time.Time) *optOrderHarness {
	t.Helper()
	initial := map[string]decimal.Decimal{}
	for k, v := range seed {
		initial[k] = decimal.MustParse(v)
	}
	ledger := account.NewLedger(initial)

	syms := []string{}
	if m != nil {
		for _, c := range m.Contracts() {
			syms = append(syms, c.Symbol)
		}
	}
	book := orderbook.New(syms)
	eng := engine.New(book, noopMargin{}, nil)
	authn := NewAuthenticator(testAPIKey, testSecret, clock)
	registry := NewRegistry(clock)
	opts := []Option{WithClock(clock), WithLedger(ledger)}
	if m != nil {
		opts = append(opts, WithOptionsMarket(m))
	}
	srv := New(eng, newTestSymbolMap(), authn, registry, opts...)
	srv.AttachHooks(book)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &optOrderHarness{
		testHarness: &testHarness{srv: ts, eng: eng, book: book, registry: registry, nowMs: clock().UnixMilli()},
		bsrv:        srv,
		ledger:      ledger,
		market:      m,
		nowFn:       clock,
	}
}

func (h *optOrderHarness) eapiDo(t *testing.T, method, path string, params url.Values) *http.Response {
	t.Helper()
	params.Set("timestamp", strconv.FormatInt(h.nowFn().UnixMilli(), 10))
	raw := params.Encode()
	req, err := http.NewRequest(method, h.srv.URL+path+"?"+raw+"&signature="+sign(testSecret, raw), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-MBX-APIKEY", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func eapiParams(v ...string) url.Values {
	p := url.Values{}
	for i := 0; i+1 < len(v); i += 2 {
		p.Set(v[i], v[i+1])
	}
	return p
}

func freeBal(t *testing.T, l *account.Ledger, asset string) decimal.Decimal {
	t.Helper()
	return l.Get(asset).Free
}

func lockedBal(t *testing.T, l *account.Ledger, asset string) decimal.Decimal {
	t.Helper()
	return l.Get(asset).Locked
}

func (h *optOrderHarness) placeOption(t *testing.T, params url.Values) (optionPlaceResponse, int) {
	t.Helper()
	resp := h.eapiDo(t, http.MethodPost, "/eapi/v1/order", params)
	out := optionPlaceResponse{}
	decode(t, resp, &out)
	return out, resp.StatusCode
}

func (h *optOrderHarness) eapiStatus(t *testing.T, method, path string, params url.Values) (apiError, int) {
	t.Helper()
	resp := h.eapiDo(t, method, path, params)
	e := apiError{}
	decode(t, resp, &e)
	return e, resp.StatusCode
}

// openCount is the number of resting (NEW/PARTIALLY_FILLED) option orders.
func (h *optOrderHarness) openCount(sym string) int {
	n := 0
	for _, r := range h.registry.OpenOrders(sym) {
		if isOptionBookSymbol(r.EngineSymbol) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Contract spec on exchangeInfo
// ---------------------------------------------------------------------------

func TestEAPIOption_ContractSpec(t *testing.T) {
	h := newOptOrderHarness(t, optTestMarket(), nil)
	resp := h.get(t, "/eapi/v1/exchangeInfo")
	var body struct {
		OptionSymbols []map[string]any `json:"optionSymbols"`
	}
	decode(t, resp, &body)
	if len(body.OptionSymbols) != 6 {
		t.Fatalf("want 6 symbols, got %d", len(body.OptionSymbols))
	}
	for _, s := range body.OptionSymbols {
		if s["exerciseStyle"] != "european" || s["settlement"] != "cash" || s["indexRule"] != "point_in_time" {
			t.Errorf("settlement spec wrong on %v: %+v", s["symbol"], s)
		}
	}
}

// ---------------------------------------------------------------------------
// Place / query / cancel lifecycle + idempotency + reserve/release
// ---------------------------------------------------------------------------

func TestEAPIOption_Lifecycle(t *testing.T) {
	sym := "BTC-261231-50000-C"
	otc := "OTC:" + sym
	h := newOptOrderHarness(t, optTestMarket(), map[string]string{"USDT": "100000", otc: "4"})
	mk, err := h.market.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}
	pin := eapiNum(mk.Price) // at the mark: inside the band by construction

	if e, _ := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", "NOPE-X-50000-C", "side", "BUY", "type", "LIMIT", "quantity", "1", "price", pin, "newClientOrderId", "x1")); e.Code != -1121 {
		t.Fatalf("unknown symbol: %+v", e)
	}

	placed, code := h.placeOption(t, eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT",
		"quantity", "2", "price", pin, "newClientOrderId", "c1"))
	if code != http.StatusOK || placed.Status != "NEW" || placed.Quantity != "2.00000000" {
		t.Fatalf("place: %+v (status %d)", placed, code)
	}
	buyID := placed.OrderID

	// A BUY reserves 2·mark of USDT.
	wantReserve := decimal.MustParse("100000").Sub(decimal.MustParse(pin).Mul(decimal.FromInt(2)))
	if got := freeBal(t, h.ledger, "USDT"); got != wantReserve {
		t.Fatalf("USDT free after buy reserve: %s want %s", got, wantReserve)
	}

	// Same newClientOrderId → duplicate (-2010), never a second order.
	if e, _ := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT", "quantity", "2", "price", pin, "newClientOrderId", "c1")); e.Code != -2010 {
		t.Fatalf("duplicate: %+v", e)
	}

	// Query by orderId.
	resp := h.eapiDo(t, http.MethodGet, "/eapi/v1/order", eapiParams("symbol", sym, "orderId", strconv.FormatInt(buyID, 10)))
	var got OptionOrderStatus
	decode(t, resp, &got)
	if got.Status != "NEW" || got.ClientOrderID != "c1" {
		t.Fatalf("query by id: %+v", got)
	}
	// Query by origClientOrderId.
	resp = h.eapiDo(t, http.MethodGet, "/eapi/v1/order", eapiParams("symbol", sym, "origClientOrderId", "c1"))
	decode(t, resp, &got)
	if got.OrderID != buyID {
		t.Fatalf("query by clientOrderId: %+v", got)
	}

	// Open orders lists it.
	resp = h.eapiDo(t, http.MethodGet, "/eapi/v1/orders", eapiParams("symbol", sym))
	var open []OptionOrderStatus
	decode(t, resp, &open)
	if len(open) != 1 || open[0].OrderID != buyID {
		t.Fatalf("openOrders: %+v", open)
	}

	// Cancel: the 2-unit quote reservation is released → free returns to 100000.
	resp = h.eapiDo(t, http.MethodDelete, "/eapi/v1/order", eapiParams("symbol", sym, "orderId", strconv.FormatInt(buyID, 10)))
	decode(t, resp, &got)
	if got.Status != "CANCELED" {
		t.Fatalf("cancel: %+v", got)
	}
	if got := freeBal(t, h.ledger, "USDT"); got != decimal.MustParse("100000") {
		t.Errorf("cancel must release the full reserve: %s (want 100000)", got)
	}
	if n := h.openCount(sym); n != 0 {
		t.Fatalf("rests after cancel: %d", n)
	}

	if e, _ := h.eapiStatus(t, http.MethodDelete, "/eapi/v1/order", eapiParams("symbol", sym, "orderId", "999999")); e.Code != -2011 {
		t.Fatalf("cancel unknown: %+v", e)
	}
}

// ---------------------------------------------------------------------------
// Matching + two-sided ledger (buy mints contracts, sell redeems them)
// ---------------------------------------------------------------------------

func TestEAPIOption_MatchingLedger(t *testing.T) {
	sym := "BTC-261231-40000-C"
	otc := "OTC:" + sym
	h := newOptOrderHarness(t, optTestMarket(), map[string]string{"USDT": "200000", otc: "5"})
	mk, err := h.market.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}
	maker := eapiNum(mk.Price)

	// A SELL larger than the account holds is rejected (-2010), no record.
	if e, _ := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", sym, "side", "SELL", "type", "LIMIT", "quantity", "500", "price", maker, "newClientOrderId", "broke")); e.Code != -2010 {
		t.Fatalf("underfunded sell: %+v", e)
	}

	// The client rests 3 SELLs at the mark. Contract reserve: 5→2.
	sell, code := h.placeOption(t, eapiParams("symbol", sym, "side", "SELL", "type", "LIMIT",
		"quantity", "3", "price", maker, "newClientOrderId", "s1"))
	if code != http.StatusOK || sell.Status != "NEW" {
		t.Fatalf("sell: %+v (status %d)", sell, code)
	}
	sellID := sell.OrderID
	if got := freeBal(t, h.ledger, otc); got != decimal.MustParse("2") {
		t.Fatalf("contract reserve: %s (want 2)", got)
	}

	// A BUY above the mark crosses the resting SELL → fills at the maker price.
	buy, code := h.placeOption(t, eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT",
		"quantity", "2", "price", eapiNum(mk.Price*1.05), "newClientOrderId", "b1"))
	if code != http.StatusOK || buy.Status != "FILLED" || buy.ExecutedQuantity != "2.00000000" {
		t.Fatalf("cross: %+v (status %d)", buy, code)
	}
	if len(buy.Fills) != 1 || buy.Fills[0].Quantity != "2.00000000" || buy.Fills[0].Price != maker {
		t.Fatalf("fills at maker price: %+v", buy.Fills)
	}

	// Cancel the SELL's remaining 1; its reservation is released (2→3).
	resp := h.eapiDo(t, http.MethodDelete, "/eapi/v1/order", eapiParams("symbol", sym, "orderId", strconv.FormatInt(sellID, 10)))
	var st OptionOrderStatus
	decode(t, resp, &st)
	if st.Status != "CANCELED" || st.ExecutedQuantity != "2.00000000" {
		t.Fatalf("sell cancel: %+v", st)
	}
	// The self-cross nets to zero: the client is both buyer and seller, so the
	// 2 units it sold were matched by the 2 it bought — the account ends back
	// at its original 5 (plus the 1 reserved sell-unit now released to free).
	if got := freeBal(t, h.ledger, otc); got != decimal.MustParse("5") {
		t.Errorf("self-cross should net to zero position: %s (want 5)", got)
	}
}

// ---------------------------------------------------------------------------
// Price band enforcement
// ---------------------------------------------------------------------------

func TestEAPIOption_PriceBand(t *testing.T) {
	sym := "BTC-261231-50000-C"
	h := newOptOrderHarness(t, optTestMarket(), map[string]string{"USDT": "100000"})
	mk, err := h.market.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", sym, "side", "SELL", "type", "LIMIT", "quantity", "1", "price", "0.00000001", "newClientOrderId", "a")); e.Code != -4164 {
		t.Fatalf("below band: %+v", e)
	}
	above := strconv.FormatFloat(mk.HighPriceLimit*2, 'f', 8, 64)
	if e, _ := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", sym, "side", "SELL", "type", "LIMIT", "quantity", "1", "price", above, "newClientOrderId", "b")); e.Code != -4164 {
		t.Fatalf("above band: %+v", e)
	}
	// Inside band is accepted. (BUY: needs only quote, which is seeded; a SELL
	// would additionally need contract holdings.)
	if _, code := h.placeOption(t, eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT",
		"quantity", "1", "price", eapiNum(mk.Price), "newClientOrderId", "ok")); code != http.StatusOK {
		t.Fatalf("inside band rejected: %d", code)
	}
}

// ---------------------------------------------------------------------------
// Expiry settle: rests canceled, cash settled in the quote asset
// ---------------------------------------------------------------------------

func TestEAPIOption_SettleSweep(t *testing.T) {
	sym := "BTC-261231-40000-C"
	otc := "OTC:" + sym
	exp := time.Date(2026, 12, 31, 8, 0, 0, 0, time.UTC)
	// ONE movable clock drives both the market and the edge, as production's
	// single wall clock does: place BEFORE expiry (allowed), advance TO
	// expiry (places after it are rejected), settle.
	now := exp.Add(-time.Hour)
	shared := func() time.Time { return now }
	m := optmarket.NewMarket(shared,
		func(u string) (float64, bool) {
			if u == "BTCUSDT" {
				return 55000, true
			}
			return 0, false
		}, 0.03, 0.02, 0.02, 0.5)
	m.SetSurface("BTCUSDT", optmarket.VolSurface{ATMVol: 0.65})
	date := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	m.AddInstrument(optmarket.NewInstrument("BTC", "BTCUSDT", "USDT", 40000, options.Call, date))

	// The client starts long 8 contracts and 1,000,000 USDT.
	h := newOptOrderHarnessClock(t, m, map[string]string{"USDT": "1000000", otc: "8"}, shared)
	mk, err := m.Mark(sym)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-expiry: rest a SELL at 0.9·mark and a BUY at 0.6·mark (both inside
	// the mark band, neither crosses), then a BUY at the mark that crosses
	// the SELL maker → 2 execute.
	if _, c := h.placeOption(t, eapiParams("symbol", sym, "side", "SELL", "type", "LIMIT",
		"quantity", "2", "price", eapiNum(mk.Price*0.9), "newClientOrderId", "s1")); c != http.StatusOK {
		t.Fatalf("rest sell: %d", c)
	}
	if _, c := h.placeOption(t, eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT",
		"quantity", "3", "price", eapiNum(mk.Price*0.6), "newClientOrderId", "b1")); c != http.StatusOK {
		t.Fatalf("rest buy: %d", c)
	}
	placed, c := h.placeOption(t, eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT",
		"quantity", "2", "price", eapiNum(mk.Price), "newClientOrderId", "b2"))
	if c != http.StatusOK || placed.Status != "FILLED" {
		t.Fatalf("cross: %+v (status %d)", placed, c)
	}

	// Self-cross nets to zero: the account still holds 8 free contracts (s1's
	// 2 redeemed = b2's 2 minted), with the 3-unit buy b1 resting.
	if got := freeBal(t, h.ledger, otc); got != decimal.MustParse("8") {
		t.Fatalf("contract free before settle: %s (want 8)", got)
	}
	if n := h.openCount(sym); n != 1 {
		t.Fatalf("resting before settle: %d (want 1: the 3-buy)", n)
	}

	// AT the settlement instant: entry is rejected.
	now = exp
	if e, sc := h.eapiStatus(t, http.MethodPost, "/eapi/v1/order",
		eapiParams("symbol", sym, "side", "BUY", "type", "LIMIT", "quantity", "1",
			"price", eapiNum(mk.Price), "newClientOrderId", "late")); e.Code != -4169 || sc != http.StatusBadRequest {
		t.Fatalf("entry at/after expiry must be rejected: %+v (status %d)", e, sc)
	}

	usdtBefore := freeBal(t, h.ledger, "USDT")
	restReserve := lockedBal(t, h.ledger, "USDT") // b1 = 3·0.5·mark
	recs := h.bsrv.SettleOptions(context.Background())
	if len(recs) != 1 || recs[0].Symbol != sym || recs[0].PerUnit != 15000 {
		t.Fatalf("settlement: %+v", recs)
	}
	// b1's quote reservation is released and the 8 held contracts cash at
	// 15000 → +120,000 USDT; the 8 expired OTC units are BURNED.
	want := usdtBefore.Add(restReserve).Add(decimal.FromFloat(15000.0 * 8.0))
	if got := freeBal(t, h.ledger, "USDT"); got != want {
		t.Errorf("USDT free after settle: %s want %s", got, want)
	}
	if got := freeBal(t, h.ledger, otc); !got.IsZero() {
		t.Errorf("expired contracts must be burned: %s free (want 0)", got)
	}
	if got := lockedBal(t, h.ledger, "USDT"); !got.IsZero() {
		t.Errorf("locked USDT after settle: %s (want 0)", got)
	}
	if n := h.openCount(sym); n != 0 {
		t.Errorf("rests after settle: %d (want 0 — canceled by sweep)", n)
	}
	// Idempotent: a second sweep settles nothing.
	if recs2 := h.bsrv.SettleOptions(context.Background()); len(recs2) != 0 {
		t.Errorf("second sweep not a no-op: %+v", recs2)
	}
}
