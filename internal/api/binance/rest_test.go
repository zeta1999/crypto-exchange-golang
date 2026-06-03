package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// noopMargin allows every order (the edge tests exercise the API surface, not
// credit policy).
type noopMargin struct{}

func (noopMargin) Validate(context.Context, *orderbook.Order) error { return nil }

// testHarness wires a real engine + order book to the Binance edge over an
// httptest server, with a fixed clock so signatures verify deterministically.
type testHarness struct {
	srv      *httptest.Server
	eng      *engine.Engine
	book     *orderbook.OrderBook
	registry *Registry
	nowMs    int64
}

func newHarness(t *testing.T, opts ...Option) *testHarness {
	t.Helper()
	nowMs := int64(1_700_000_000_000)
	clock := func() time.Time { return time.UnixMilli(nowMs).UTC() }

	book := orderbook.New([]string{"BTC-USD", "ETH-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	symbols := newTestSymbolMap()
	authn := NewAuthenticator(testAPIKey, testSecret, clock)
	registry := NewRegistry(clock)
	bsrv := New(eng, symbols, authn, registry, append([]Option{WithClock(clock)}, opts...)...)
	bsrv.AttachHooks(book)

	ts := httptest.NewServer(bsrv.Handler())
	t.Cleanup(ts.Close)
	return &testHarness{srv: ts, eng: eng, book: book, registry: registry, nowMs: nowMs}
}

// signedDo issues a SIGNED request (key header + computed signature over the
// query) and returns the response.
func (h *testHarness) signedDo(t *testing.T, method, path string, params url.Values) *http.Response {
	t.Helper()
	params.Set("timestamp", "1700000000000")
	raw := params.Encode()
	sig := sign(testSecret, raw)
	req, err := http.NewRequest(method, h.srv.URL+path+"?"+raw+"&signature="+sig, nil)
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

func (h *testHarness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(h.srv.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestPublicEndpoints(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/api/v3/ping")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.get(t, "/api/v3/time")
	var tm struct {
		ServerTime int64 `json:"serverTime"`
	}
	decode(t, resp, &tm)
	if tm.ServerTime != h.nowMs {
		t.Fatalf("serverTime = %d want %d", tm.ServerTime, h.nowMs)
	}
}

func TestDepthAndTicker(t *testing.T) {
	h := newHarness(t)
	// Seed a book: resting bid at 100, ask at 101 (different engine IDs so they
	// don't interfere with the edge's binance: namespace).
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "seed-bid", Instrument: "BTC-USD", Side: orderbook.SideBuy,
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("5"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "seed-ask", Instrument: "BTC-USD", Side: orderbook.SideSell,
		Price: decimal.MustParse("101"), Volume: decimal.MustParse("3"),
	}); err != nil {
		t.Fatal(err)
	}

	resp := h.get(t, "/api/v3/depth?symbol=BTCUSDT&limit=10")
	var depth depthResponse
	decode(t, resp, &depth)
	if len(depth.Bids) != 1 || depth.Bids[0][0] != "100.00000000" || depth.Bids[0][1] != "5.00000000" {
		t.Fatalf("bids = %v", depth.Bids)
	}
	if len(depth.Asks) != 1 || depth.Asks[0][0] != "101.00000000" {
		t.Fatalf("asks = %v", depth.Asks)
	}

	// No trade yet → ticker uses mid of 100/101 = 100.5.
	resp = h.get(t, "/api/v3/ticker/price?symbol=BTCUSDT")
	var tk tickerPriceResponse
	decode(t, resp, &tk)
	if tk.Symbol != "BTCUSDT" || tk.Price != "100.50000000" {
		t.Fatalf("ticker = %+v want mid 100.5", tk)
	}

	// Unknown symbol → -1121.
	resp = h.get(t, "/api/v3/depth?symbol=DOGEUSDT")
	var ae apiError
	decode(t, resp, &ae)
	if ae.Code != codeInvalidSymbol {
		t.Fatalf("unknown symbol code = %d want %d", ae.Code, codeInvalidSymbol)
	}
}

func TestPlaceLimitOrder_AckShape(t *testing.T) {
	h := newHarness(t)
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("side", "BUY")
	params.Set("type", "LIMIT")
	params.Set("timeInForce", "GTC")
	params.Set("quantity", "2")
	params.Set("price", "100")
	params.Set("newClientOrderId", "client-abc")

	resp := h.signedDo(t, http.MethodPost, "/api/v3/order", params)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	var ord orderResponse
	decode(t, resp, &ord)
	if ord.OrderID == 0 {
		t.Fatalf("orderId = 0")
	}
	if ord.Symbol != "BTCUSDT" || ord.Side != "BUY" || ord.Type != "LIMIT" {
		t.Fatalf("bad order shape: %+v", ord)
	}
	if ord.ClientOrderID != "client-abc" {
		t.Fatalf("clientOrderId = %q want client-abc", ord.ClientOrderID)
	}
	if ord.Status != statusNew {
		t.Fatalf("status = %q want NEW (resting, no counterparty)", ord.Status)
	}
	if ord.OrigQty != "2.00000000" {
		t.Fatalf("origQty = %q", ord.OrigQty)
	}

	// openOrders reflects the resting order.
	resp = h.signedDo(t, http.MethodGet, "/api/v3/openOrders", url.Values{})
	var open []openOrderResponse
	decode(t, resp, &open)
	if len(open) != 1 || open[0].OrderID != ord.OrderID {
		t.Fatalf("openOrders = %v want [orderId %d]", open, ord.OrderID)
	}

	// Cancel it.
	cancelParams := url.Values{}
	cancelParams.Set("symbol", "BTCUSDT")
	cancelParams.Set("orderId", strconv.FormatInt(ord.OrderID, 10))
	resp = h.signedDo(t, http.MethodDelete, "/api/v3/order", cancelParams)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	var canc canceledOrderResponse
	decode(t, resp, &canc)
	if canc.Status != statusCanceled {
		t.Fatalf("cancel status = %q want CANCELED", canc.Status)
	}

	// openOrders now empty.
	resp = h.signedDo(t, http.MethodGet, "/api/v3/openOrders", url.Values{})
	open = nil
	decode(t, resp, &open)
	if len(open) != 0 {
		t.Fatalf("openOrders after cancel = %d want 0", len(open))
	}
}

func TestRestingOrderFilledByCounterOrder(t *testing.T) {
	h := newHarness(t)

	// Place a resting BUY limit at 100 for qty 2 via the edge.
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("side", "BUY")
	params.Set("type", "LIMIT")
	params.Set("quantity", "2")
	params.Set("price", "100")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/order", params)
	var ord orderResponse
	decode(t, resp, &ord)

	// A counter SELL crosses it (direct book op simulating other flow).
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "counter-sell", Instrument: "BTC-USD", Side: orderbook.SideSell,
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("2"),
	}); err != nil {
		t.Fatal(err)
	}

	// The trade hook should have updated our order to FILLED.
	snap, ok := h.registry.snapshot(ord.OrderID)
	if !ok {
		t.Fatal("record vanished")
	}
	if snap.Status != statusFilled {
		t.Fatalf("status = %q want FILLED after counter fill", snap.Status)
	}
	if snap.ExecutedQty.Cmp(decimal.MustParse("2")) != 0 {
		t.Fatalf("executedQty = %s want 2", snap.ExecutedQty.StringPrec(2))
	}
}

func TestMarketOrderFillsImmediately(t *testing.T) {
	h := newHarness(t)
	// Resting ask liquidity at 101.
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "seed-ask", Instrument: "BTC-USD", Side: orderbook.SideSell,
		Price: decimal.MustParse("101"), Volume: decimal.MustParse("5"),
	}); err != nil {
		t.Fatal(err)
	}
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("side", "BUY")
	params.Set("type", "MARKET")
	params.Set("quantity", "3")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/order", params)
	var ord orderResponse
	decode(t, resp, &ord)
	if ord.Status != statusFilled {
		t.Fatalf("market order status = %q want FILLED", ord.Status)
	}
	if len(ord.Fills) == 0 {
		t.Fatalf("expected fills array populated")
	}
	if ord.ExecutedQty != "3.00000000" {
		t.Fatalf("executedQty = %q want 3", ord.ExecutedQty)
	}
}

func TestSignedEndpointRejectsUnsigned(t *testing.T) {
	h := newHarness(t)
	// No signature, no key.
	resp, err := http.Post(h.srv.URL+"/api/v3/order?symbol=BTCUSDT&side=BUY&type=MARKET&quantity=1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d want 401", resp.StatusCode)
	}
	var ae apiError
	decode(t, resp, &ae)
	if ae.Code != codeBadAPIKeyFmt {
		t.Fatalf("code = %d want %d", ae.Code, codeBadAPIKeyFmt)
	}
}

func TestCancelUnknownOrder(t *testing.T) {
	h := newHarness(t)
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("orderId", "999999")
	resp := h.signedDo(t, http.MethodDelete, "/api/v3/order", params)
	var ae apiError
	decode(t, resp, &ae)
	if ae.Code != codeUnknownOrder {
		t.Fatalf("code = %d want %d", ae.Code, codeUnknownOrder)
	}
}

func TestAccountStub(t *testing.T) {
	h := newHarness(t)
	resp := h.signedDo(t, http.MethodGet, "/api/v3/account", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("account status = %d", resp.StatusCode)
	}
	var acct accountResponse
	decode(t, resp, &acct)
	if !acct.CanTrade {
		t.Fatalf("canTrade = false")
	}
	if acct.Balances == nil {
		t.Fatalf("balances must be non-nil (empty stub)")
	}
}
