package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// testHarness wires a real engine + order book to the Coinbase edge over an
// httptest server, with a fixed clock so signatures verify deterministically.
type testHarness struct {
	srv      *httptest.Server
	eng      *engine.Engine
	book     *orderbook.OrderBook
	registry *Registry
	baseURL  string
	tsSec    int64
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	tsSec := int64(1_700_000_000)
	clock := func() time.Time { return time.Unix(tsSec, 0).UTC() }

	book := orderbook.New([]string{"BTC-USD", "ETH-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	products := NewProducts([]string{"BTC-USD", "ETH-USD"})
	authn := NewAuthenticator(testAPIKey, testSecret, testPassphrase, clock)
	registry := NewRegistry(clock)
	csrv := New(eng, products, authn, registry, WithClock(clock))
	csrv.AttachHooks(book)

	ts := httptest.NewServer(csrv.Handler())
	t.Cleanup(ts.Close)
	return &testHarness{srv: ts, eng: eng, book: book, registry: registry, baseURL: ts.URL, tsSec: tsSec}
}

// signedDo issues a SIGNED request. path is the request path including any
// query string; body is the raw request body (empty for GETs). The signature
// covers ts+method+path+body.
func (h *testHarness) signedDo(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	tsStr := strconv.FormatInt(h.tsSec, 10)
	req, err := http.NewRequest(method, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("CB-ACCESS-KEY", testAPIKey)
	req.Header.Set("CB-ACCESS-TIMESTAMP", tsStr)
	req.Header.Set("CB-ACCESS-PASSPHRASE", testPassphrase)
	req.Header.Set("CB-ACCESS-SIGN", signMessage(testSecret, tsStr, method, path, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func (h *testHarness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(h.baseURL + path)
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

func TestTimeEndpoint(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/api/v3/brokerage/time")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("time status = %d", resp.StatusCode)
	}
	var tm struct {
		ISO          string `json:"iso"`
		EpochSeconds string `json:"epochSeconds"`
		EpochMillis  string `json:"epochMillis"`
	}
	decode(t, resp, &tm)
	if tm.EpochSeconds != strconv.FormatInt(h.tsSec, 10) {
		t.Fatalf("epochSeconds = %q want %d", tm.EpochSeconds, h.tsSec)
	}
	if tm.ISO == "" || tm.EpochMillis == "" {
		t.Fatalf("missing iso/epochMillis: %+v", tm)
	}
}

func TestProductBookAndTicker(t *testing.T) {
	h := newHarness(t)
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

	resp := h.get(t, "/api/v3/brokerage/product_book?product_id=BTC-USD&limit=10")
	var pb productBookResponse
	decode(t, resp, &pb)
	if pb.PriceBook.ProductID != "BTC-USD" {
		t.Fatalf("product_id = %q", pb.PriceBook.ProductID)
	}
	if len(pb.PriceBook.Bids) != 1 || pb.PriceBook.Bids[0].Price != "100.00000000" || pb.PriceBook.Bids[0].Size != "5.00000000" {
		t.Fatalf("bids = %v", pb.PriceBook.Bids)
	}
	if len(pb.PriceBook.Asks) != 1 || pb.PriceBook.Asks[0].Price != "101.00000000" {
		t.Fatalf("asks = %v", pb.PriceBook.Asks)
	}

	// Product ticker: no trade yet → mid of 100/101 = 100.5.
	resp = h.get(t, "/api/v3/brokerage/products/BTC-USD")
	var pr productResponse
	decode(t, resp, &pr)
	if pr.ProductID != "BTC-USD" || pr.Price != "100.50000000" || pr.Status != "online" {
		t.Fatalf("product = %+v want price 100.5 online", pr)
	}
	if pr.BaseCur != "BTC" || pr.QuoteCur != "USD" {
		t.Fatalf("currencies = %s/%s want BTC/USD", pr.BaseCur, pr.QuoteCur)
	}

	// Unknown product → INVALID_PRODUCT_ID.
	resp = h.get(t, "/api/v3/brokerage/product_book?product_id=DOGE-USD")
	var ae apiError
	decode(t, resp, &ae)
	if ae.Err != errUnknownProduct {
		t.Fatalf("unknown product err = %q want %q", ae.Err, errUnknownProduct)
	}
}

// limitOrderBody builds a limit_limit_gtc create-order JSON body.
func limitOrderBody(clientID, productID, side, size, price string) string {
	v := map[string]interface{}{
		"client_order_id": clientID,
		"product_id":      productID,
		"side":            side,
		"order_configuration": map[string]interface{}{
			"limit_limit_gtc": map[string]interface{}{
				"base_size":   size,
				"limit_price": price,
				"post_only":   false,
			},
		},
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func marketOrderBody(clientID, productID, side, size string) string {
	v := map[string]interface{}{
		"client_order_id": clientID,
		"product_id":      productID,
		"side":            side,
		"order_configuration": map[string]interface{}{
			"market_market_ioc": map[string]interface{}{"base_size": size},
		},
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCreateLimitOrder_SuccessShape(t *testing.T) {
	h := newHarness(t)
	body := limitOrderBody("client-abc", "BTC-USD", "BUY", "2", "100")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var cr createOrderResponse
	decode(t, resp, &cr)
	if !cr.Success || cr.SuccessResponse == nil {
		t.Fatalf("expected success, got %+v", cr)
	}
	if cr.SuccessResponse.OrderID == "" {
		t.Fatalf("empty order_id")
	}
	if cr.SuccessResponse.ProductID != "BTC-USD" || cr.SuccessResponse.Side != "BUY" {
		t.Fatalf("bad success_response: %+v", cr.SuccessResponse)
	}
	if cr.SuccessResponse.ClientOrderID != "client-abc" {
		t.Fatalf("client_order_id = %q want client-abc", cr.SuccessResponse.ClientOrderID)
	}
	if cr.OrderConfiguration == nil || cr.OrderConfiguration.LimitLimitGTC == nil {
		t.Fatalf("expected echoed limit_limit_gtc config: %+v", cr.OrderConfiguration)
	}

	// historical/batch reflects the OPEN order.
	path := "/api/v3/brokerage/orders/historical/batch?product_id=BTC-USD&order_status=OPEN"
	resp = h.signedDo(t, http.MethodGet, path, "")
	var hb historicalBatchResponse
	decode(t, resp, &hb)
	if len(hb.Orders) != 1 || hb.Orders[0].OrderID != cr.SuccessResponse.OrderID {
		t.Fatalf("historical batch = %+v want one open order %s", hb.Orders, cr.SuccessResponse.OrderID)
	}
	if hb.Orders[0].Status != statusOpen {
		t.Fatalf("order status = %q want OPEN", hb.Orders[0].Status)
	}

	// historical/{order_id} single lookup.
	resp = h.signedDo(t, http.MethodGet, "/api/v3/brokerage/orders/historical/"+cr.SuccessResponse.OrderID, "")
	var so singleOrderResponse
	decode(t, resp, &so)
	if so.Order.OrderID != cr.SuccessResponse.OrderID {
		t.Fatalf("single order = %+v", so.Order)
	}
}

func TestBatchCancel(t *testing.T) {
	h := newHarness(t)
	body := limitOrderBody("c1", "BTC-USD", "BUY", "1", "100")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	var cr createOrderResponse
	decode(t, resp, &cr)
	oid := cr.SuccessResponse.OrderID

	cancelBody, _ := json.Marshal(map[string]interface{}{"order_ids": []string{oid, "emu-999"}})
	resp = h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders/batch_cancel", string(cancelBody))
	var bc batchCancelResponse
	decode(t, resp, &bc)
	if len(bc.Results) != 2 {
		t.Fatalf("results = %d want 2", len(bc.Results))
	}
	if !bc.Results[0].Success || bc.Results[0].OrderID != oid {
		t.Fatalf("first result = %+v want success for %s", bc.Results[0], oid)
	}
	if bc.Results[1].Success || bc.Results[1].FailureReason != errUnknownCancel {
		t.Fatalf("second result = %+v want unknown cancel", bc.Results[1])
	}

	// The cancelled order drops out of open orders.
	resp = h.signedDo(t, http.MethodGet, "/api/v3/brokerage/orders/historical/batch?order_status=OPEN", "")
	var hb historicalBatchResponse
	decode(t, resp, &hb)
	if len(hb.Orders) != 0 {
		t.Fatalf("open orders after cancel = %d want 0", len(hb.Orders))
	}
}

func TestRestingOrderFilledByCounterOrder(t *testing.T) {
	h := newHarness(t)
	body := limitOrderBody("c1", "BTC-USD", "BUY", "2", "100")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	var cr createOrderResponse
	decode(t, resp, &cr)
	oid := cr.SuccessResponse.OrderID

	// A counter SELL crosses it (direct book op simulating other flow).
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "counter-sell", Instrument: "BTC-USD", Side: orderbook.SideSell,
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("2"),
	}); err != nil {
		t.Fatal(err)
	}

	snap, ok := h.registry.snapshot(oid)
	if !ok {
		t.Fatal("record vanished")
	}
	if snap.Status != statusFilled {
		t.Fatalf("status = %q want FILLED after counter fill", snap.Status)
	}
	if snap.FilledSize.Cmp(decimal.MustParse("2")) != 0 {
		t.Fatalf("filledSize = %s want 2", snap.FilledSize.StringPrec(2))
	}

	// historical/{order_id} reflects the filled state.
	resp = h.signedDo(t, http.MethodGet, "/api/v3/brokerage/orders/historical/"+oid, "")
	var so singleOrderResponse
	decode(t, resp, &so)
	if so.Order.Status != statusFilled || so.Order.FilledSize != "2.00000000" {
		t.Fatalf("order view = %+v want FILLED size 2", so.Order)
	}
	if so.Order.AverageFilledPrice != "100.00000000" {
		t.Fatalf("avg filled price = %q want 100", so.Order.AverageFilledPrice)
	}
}

func TestMarketOrderFillsImmediately(t *testing.T) {
	h := newHarness(t)
	if _, err := h.book.AddLimitOrder(&orderbook.Order{
		ID: "seed-ask", Instrument: "BTC-USD", Side: orderbook.SideSell,
		Price: decimal.MustParse("101"), Volume: decimal.MustParse("5"),
	}); err != nil {
		t.Fatal(err)
	}
	body := marketOrderBody("m1", "BTC-USD", "BUY", "3")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	var cr createOrderResponse
	decode(t, resp, &cr)
	if !cr.Success {
		t.Fatalf("market create failed: %+v", cr.ErrorResponse)
	}
	snap, _ := h.registry.snapshot(cr.SuccessResponse.OrderID)
	if snap.Status != statusFilled {
		t.Fatalf("market order status = %q want FILLED", snap.Status)
	}
	if snap.FilledSize.Cmp(decimal.MustParse("3")) != 0 {
		t.Fatalf("filledSize = %s want 3", snap.FilledSize.StringPrec(2))
	}
}

func TestSignedEndpointRejectsUnsigned(t *testing.T) {
	h := newHarness(t)
	body := marketOrderBody("u1", "BTC-USD", "BUY", "1")
	resp, err := http.Post(h.baseURL+"/api/v3/brokerage/orders", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned status = %d want 401", resp.StatusCode)
	}
	var ae apiError
	decode(t, resp, &ae)
	if ae.Err != errUnauthorized {
		t.Fatalf("err = %q want %q", ae.Err, errUnauthorized)
	}
}

func TestPublicEndpointsNeedNoAuth(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{
		"/api/v3/brokerage/time",
		"/api/v3/brokerage/product_book?product_id=BTC-USD",
		"/api/v3/brokerage/products/BTC-USD",
	} {
		resp := h.get(t, p)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("public %s status = %d want 200", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestMalformedNumericBodyIsCleanError(t *testing.T) {
	h := newHarness(t)
	// base_size = "NaN" must NOT panic (no ParseFloat fallback) — clean 400.
	for _, sz := range []string{"NaN", "Inf", "1e400", "abc", ""} {
		body := limitOrderBody("c-"+sz, "BTC-USD", "BUY", sz, "100")
		resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("base_size=%q status = %d want 400", sz, resp.StatusCode)
		}
		var ae apiError
		decode(t, resp, &ae)
		if ae.Err != errInvalidRequest {
			t.Fatalf("base_size=%q err = %q want %q", sz, ae.Err, errInvalidRequest)
		}
	}
}

func TestMalformedJSONBodyIsCleanError(t *testing.T) {
	h := newHarness(t)
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", "{not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json status = %d want 400", resp.StatusCode)
	}
}

func TestAccountsStub(t *testing.T) {
	h := newHarness(t)
	resp := h.signedDo(t, http.MethodGet, "/api/v3/brokerage/accounts", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accounts status = %d", resp.StatusCode)
	}
	var ar accountsResponse
	decode(t, resp, &ar)
	if ar.Accounts == nil {
		t.Fatalf("accounts must be non-nil (empty stub)")
	}
}

func TestCreateOrderRejectsUnknownProduct(t *testing.T) {
	h := newHarness(t)
	body := limitOrderBody("c1", "DOGE-USD", "BUY", "1", "100")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown product status = %d want 400", resp.StatusCode)
	}
	var ae apiError
	decode(t, resp, &ae)
	if ae.Err != errUnknownProduct {
		t.Fatalf("err = %q want %q", ae.Err, errUnknownProduct)
	}
}

func TestCreateOrderRejectsBothConfigs(t *testing.T) {
	h := newHarness(t)
	v := map[string]interface{}{
		"client_order_id": "c1",
		"product_id":      "BTC-USD",
		"side":            "BUY",
		"order_configuration": map[string]interface{}{
			"limit_limit_gtc":   map[string]interface{}{"base_size": "1", "limit_price": "100"},
			"market_market_ioc": map[string]interface{}{"base_size": "1"},
		},
	}
	b, _ := json.Marshal(v)
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", string(b))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both configs status = %d want 400", resp.StatusCode)
	}
}
