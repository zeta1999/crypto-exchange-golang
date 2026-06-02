package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// wsHarness wires a real engine + book + Binance edge over an httptest server
// with the depth ticker running, exposing the Server for direct calls.
type wsHarness struct {
	srv      *httptest.Server
	bsrv     *Server
	book     *orderbook.OrderBook
	registry *Registry
	nowMs    int64
}

func newWSHarness(t *testing.T) *wsHarness {
	t.Helper()
	nowMs := int64(1_700_000_000_000)
	clock := func() time.Time { return time.UnixMilli(nowMs).UTC() }

	book := orderbook.New([]string{"BTC-USD", "ETH-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	symbols := newTestSymbolMap()
	authn := NewAuthenticator(testAPIKey, testSecret, clock)
	registry := NewRegistry(clock)
	bsrv := New(eng, symbols, authn, registry, WithClock(clock))
	bsrv.AttachHooks(book)

	ctx, cancel := context.WithCancel(context.Background())
	go bsrv.Start(ctx)
	t.Cleanup(cancel)

	ts := httptest.NewServer(bsrv.Handler())
	t.Cleanup(ts.Close)
	return &wsHarness{srv: ts, bsrv: bsrv, book: book, registry: registry, nowMs: nowMs}
}

// wsURL converts the httptest http:// base URL to a ws:// dial URL for path.
func (h *wsHarness) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + path
}

func (h *wsHarness) dial(t *testing.T, path string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(h.wsURL(path), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readJSON reads one text frame within a deadline and unmarshals it.
func readJSON(t *testing.T, c *websocket.Conn, v interface{}) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
}

func TestWSMarketTradeRaw(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t, "/ws/btcusdt@trade")

	// Cause a trade: resting ask + crossing buy directly on the book.
	mustAdd(t, h.book, "seed-ask", orderbook.SideSell, "101", "5")
	mustAdd(t, h.book, "taker-buy", orderbook.SideBuy, "101", "2")

	var ev tradeEvent
	readJSON(t, c, &ev)
	if ev.EventType != "trade" || ev.Symbol != "BTCUSDT" {
		t.Fatalf("trade event = %+v", ev)
	}
	if ev.Price != "101.00000000" || ev.Quantity != "2.00000000" {
		t.Fatalf("price/qty = %s/%s", ev.Price, ev.Quantity)
	}
	if ev.TradeID == 0 {
		t.Fatalf("trade id should be monotonic non-zero")
	}
	if !ev.Ignore {
		t.Fatalf("M (ignore) should be true")
	}
}

func TestWSMarketTradeCombined(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t, "/stream?streams=btcusdt@trade")

	mustAdd(t, h.book, "seed-ask", orderbook.SideSell, "100", "5")
	mustAdd(t, h.book, "taker-buy", orderbook.SideBuy, "100", "1")

	var frame struct {
		Stream string     `json:"stream"`
		Data   tradeEvent `json:"data"`
	}
	readJSON(t, c, &frame)
	if frame.Stream != "btcusdt@trade" {
		t.Fatalf("stream = %q", frame.Stream)
	}
	if frame.Data.EventType != "trade" || frame.Data.Quantity != "1.00000000" {
		t.Fatalf("data = %+v", frame.Data)
	}
}

func TestWSDepth20(t *testing.T) {
	h := newWSHarness(t)
	// Seed a book BEFORE subscribing so the immediate snapshot has content.
	mustAdd(t, h.book, "seed-bid", orderbook.SideBuy, "100", "5")
	mustAdd(t, h.book, "seed-ask", orderbook.SideSell, "101", "3")

	c := h.dial(t, "/ws/btcusdt@depth20")
	var ev depthEvent
	readJSON(t, c, &ev) // immediate snapshot on subscribe
	if len(ev.Bids) != 1 || ev.Bids[0][0] != "100.00000000" || ev.Bids[0][1] != "5.00000000" {
		t.Fatalf("bids = %v", ev.Bids)
	}
	if len(ev.Asks) != 1 || ev.Asks[0][0] != "101.00000000" {
		t.Fatalf("asks = %v", ev.Asks)
	}
	if ev.LastUpdateID == 0 {
		t.Fatalf("lastUpdateId = 0")
	}

	// And the ticker keeps pushing — a second frame should arrive.
	var ev2 depthEvent
	readJSON(t, c, &ev2)
	if len(ev2.Bids) != 1 {
		t.Fatalf("ticker depth bids = %v", ev2.Bids)
	}
}

func TestWSDepth20WithInterval(t *testing.T) {
	h := newWSHarness(t)
	mustAdd(t, h.book, "seed-bid", orderbook.SideBuy, "100", "5")
	// The "@100ms" suffix variant must parse and behave identically.
	c := h.dial(t, "/ws/btcusdt@depth20@100ms")
	var ev depthEvent
	readJSON(t, c, &ev)
	if len(ev.Bids) != 1 {
		t.Fatalf("bids = %v", ev.Bids)
	}
}

func TestWSUnknownStreamRejected(t *testing.T) {
	h := newWSHarness(t)
	_, resp, err := websocket.DefaultDialer.Dial(h.wsURL("/ws/dogeusdt@trade"), nil)
	if err == nil {
		t.Fatal("expected dial to fail for unknown symbol")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %v want 400", resp)
	}
}

func TestListenKeyLifecycle(t *testing.T) {
	h := newWSHarness(t)

	// POST without api key -> 401.
	resp, err := http.Post(h.srv.URL+"/api/v3/userDataStream", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key POST status = %d want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// POST with api key -> listenKey.
	key := h.postListenKey(t)
	if key == "" {
		t.Fatal("empty listenKey")
	}

	// PUT keepalive ok.
	if st := h.userDataStream(t, http.MethodPut, key); st != http.StatusOK {
		t.Fatalf("PUT keepalive status = %d", st)
	}
	// PUT bad key -> error.
	if st := h.userDataStream(t, http.MethodPut, "deadbeef"); st == http.StatusOK {
		t.Fatalf("PUT bad key should fail, got %d", st)
	}
	// DELETE ok.
	if st := h.userDataStream(t, http.MethodDelete, key); st != http.StatusOK {
		t.Fatalf("DELETE status = %d", st)
	}
	// DELETE again -> error (already gone).
	if st := h.userDataStream(t, http.MethodDelete, key); st == http.StatusOK {
		t.Fatalf("DELETE of gone key should fail, got %d", st)
	}
}

func TestWSUserDataExecutionReport(t *testing.T) {
	h := newWSHarness(t)
	key := h.postListenKey(t)
	c := h.dial(t, "/ws/"+key)

	// Seed resting ask so a market BUY fills immediately.
	mustAdd(t, h.book, "seed-ask", orderbook.SideSell, "101", "5")

	// Place a MARKET BUY via the signed REST handler.
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("side", "BUY")
	params.Set("type", "MARKET")
	params.Set("quantity", "3")
	resp := h.signedDo(t, http.MethodPost, "/api/v3/order", params)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// First report is NEW (emitted before placement), then TRADE/FILLED.
	var first executionReport
	readJSON(t, c, &first)
	if first.EventType != "executionReport" || first.Symbol != "BTCUSDT" {
		t.Fatalf("first report = %+v", first)
	}
	if first.OrderStatus != statusNew || first.ExecType != execTypeNew {
		t.Fatalf("first report X/x = %s/%s want NEW/NEW", first.OrderStatus, first.ExecType)
	}

	var filled executionReport
	readJSON(t, c, &filled)
	if filled.ExecType != execTypeTrade {
		t.Fatalf("second report x = %s want TRADE", filled.ExecType)
	}
	if filled.OrderStatus != statusFilled {
		t.Fatalf("second report X = %s want FILLED", filled.OrderStatus)
	}
	if filled.Side != "BUY" || filled.OrderType != "MARKET" {
		t.Fatalf("report side/type = %s/%s", filled.Side, filled.OrderType)
	}
	if filled.CumFilledQty != "3.00000000" {
		t.Fatalf("cumFilledQty = %s want 3", filled.CumFilledQty)
	}
}

func TestWSSlowConsumerCleanedUp(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t, "/ws/btcusdt@trade")
	// Close the client immediately; the server read pump should detect it and
	// unsubscribe. Subsequent trades must not deadlock the book hook.
	_ = c.Close()

	// Give the read pump a moment to observe the close, then fire many trades.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.bsrv.broadcaster.hasMarketSubscribers("btcusdt@trade") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The book hook must complete without blocking even if a stale conn lingered.
	done := make(chan struct{})
	go func() {
		mustAdd(t, h.book, "seed-ask", orderbook.SideSell, "101", "5")
		mustAdd(t, h.book, "taker-buy", orderbook.SideBuy, "101", "2")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("book hook blocked on a disconnected WS consumer")
	}
}

// --- test helpers ---

func mustAdd(t *testing.T, book *orderbook.OrderBook, id string, side orderbook.Side, price, vol string) {
	t.Helper()
	if _, err := book.AddLimitOrder(&orderbook.Order{
		ID: id, Instrument: "BTC-USD", Side: side,
		Price: decimal.MustParse(price), Volume: decimal.MustParse(vol),
	}); err != nil {
		t.Fatalf("add %s: %v", id, err)
	}
}

func (h *wsHarness) postListenKey(t *testing.T) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v3/userDataStream", nil)
	req.Header.Set("X-MBX-APIKEY", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post listenKey: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post listenKey status = %d", resp.StatusCode)
	}
	var out struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.ListenKey
}

func (h *wsHarness) userDataStream(t *testing.T, method, key string) int {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+"/api/v3/userDataStream?listenKey="+key, nil)
	req.Header.Set("X-MBX-APIKEY", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s userDataStream: %v", method, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// signedDo mirrors the REST harness's signed request for the WS harness.
func (h *wsHarness) signedDo(t *testing.T, method, path string, params url.Values) *http.Response {
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
