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

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// wsHarness wires a real engine + book + Coinbase edge over an httptest server
// with the level2 ticker running, exposing the Server for direct calls.
type wsHarness struct {
	srv   *httptest.Server
	csrv  *Server
	book  *orderbook.OrderBook
	tsSec int64
}

func newWSHarness(t *testing.T, opts ...Option) *wsHarness {
	t.Helper()
	tsSec := int64(1_700_000_000)
	clock := func() time.Time { return time.Unix(tsSec, 0).UTC() }

	book := orderbook.New([]string{"BTC-USD", "ETH-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	products := NewProducts([]string{"BTC-USD", "ETH-USD"})
	authn := NewAuthenticator(testAPIKey, testSecret, testPassphrase, clock)
	registry := NewRegistry(clock)
	csrv := New(eng, products, authn, registry, append([]Option{WithClock(clock)}, opts...)...)
	csrv.AttachHooks(book)

	ctx, cancel := context.WithCancel(context.Background())
	go csrv.Start(ctx)
	t.Cleanup(cancel)

	ts := httptest.NewServer(csrv.Handler())
	t.Cleanup(ts.Close)
	return &wsHarness{srv: ts, csrv: csrv, book: book, tsSec: tsSec}
}

func (h *wsHarness) wsURL() string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws"
}

func (h *wsHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(h.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func wsSend(t *testing.T, c *websocket.Conn, v interface{}) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// wsReadEnvelope reads frames until one matching channel arrives (skipping acks
// and unrelated frames), within a deadline.
func wsReadChannel(t *testing.T, c *websocket.Conn, channel string) wsRawEnvelope {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = c.SetReadDeadline(deadline)
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read waiting for %q: %v", channel, err)
		}
		var env wsRawEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal %q: %v", data, err)
		}
		if env.Channel == channel {
			return env
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for channel %q", channel)
		}
	}
}

// wsReadFrame reads one raw frame and unmarshals it into v.
func wsReadFrame(t *testing.T, c *websocket.Conn, v interface{}) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
}

// wsRawEnvelope mirrors the wire envelope with raw events for per-test decoding.
type wsRawEnvelope struct {
	Channel     string            `json:"channel"`
	Timestamp   string            `json:"timestamp"`
	SequenceNum uint64            `json:"sequence_num"`
	Events      []json.RawMessage `json:"events"`
}

func wsAddOrder(t *testing.T, book *orderbook.OrderBook, id string, side orderbook.Side, price, vol string) {
	t.Helper()
	if _, err := book.AddLimitOrder(&orderbook.Order{
		ID: id, Instrument: "BTC-USD", Side: side,
		Price: decimal.MustParse(price), Volume: decimal.MustParse(vol),
	}); err != nil {
		t.Fatalf("add %s: %v", id, err)
	}
}

func TestWSLevel2Snapshot(t *testing.T) {
	h := newWSHarness(t)
	// Seed a book BEFORE subscribing so the snapshot has content.
	wsAddOrder(t, h.book, "seed-bid", orderbook.SideBuy, "100", "5")
	wsAddOrder(t, h.book, "seed-ask", orderbook.SideSell, "101", "3")

	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "level2",
		"product_ids": []string{"BTC-USD"},
	})

	env := wsReadChannel(t, c, "l2_data")
	if len(env.Events) != 1 {
		t.Fatalf("events len = %d", len(env.Events))
	}
	var ev l2Event
	if err := json.Unmarshal(env.Events[0], &ev); err != nil {
		t.Fatalf("decode l2 event: %v", err)
	}
	if ev.Type != "snapshot" || ev.ProductID != "BTC-USD" {
		t.Fatalf("event = %+v", ev)
	}
	var sawBid, sawOffer bool
	for _, u := range ev.Updates {
		switch u.Side {
		case "bid":
			sawBid = true
			if u.PriceLevel != "100.00000000" || u.NewQuantity != "5.00000000" {
				t.Fatalf("bid update = %+v", u)
			}
		case "offer":
			sawOffer = true
			if u.PriceLevel != "101.00000000" {
				t.Fatalf("offer update = %+v", u)
			}
		default:
			t.Fatalf("unexpected side %q", u.Side)
		}
	}
	if !sawBid || !sawOffer {
		t.Fatalf("missing bid/offer: %+v", ev.Updates)
	}
}

func TestWSMarketTradesAggressorSell(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	// Read the subscriptions ack so the subscription is registered before we
	// trigger the trade.
	_ = wsReadChannel(t, c, "subscriptions")

	// Resting bid lifted by an aggressing SELL -> aggressor side SELL.
	wsAddOrder(t, h.book, "resting-bid", orderbook.SideBuy, "100", "5")
	wsAddOrder(t, h.book, "taker-sell", orderbook.SideSell, "100", "2")

	env := wsReadChannel(t, c, "market_trades")
	var ev mtEvent
	if err := json.Unmarshal(env.Events[0], &ev); err != nil {
		t.Fatalf("decode mt event: %v", err)
	}
	if len(ev.Trades) != 1 {
		t.Fatalf("trades = %+v", ev.Trades)
	}
	tr := ev.Trades[0]
	if tr.Side != "SELL" {
		t.Fatalf("aggressor side = %q want SELL", tr.Side)
	}
	if tr.ProductID != "BTC-USD" || tr.Price != "100.00000000" || tr.Size != "2.00000000" {
		t.Fatalf("trade = %+v", tr)
	}
	if tr.TradeID == "" {
		t.Fatalf("trade_id empty")
	}
}

// TestWSUserFillReportDelay verifies WithFillDelay holds back the fill-driven
// user-channel update by the configured latency, while the initial OPEN (from
// the REST create) stays prompt. A resting limit order keeps the prompt OPEN
// (cum 0) cleanly distinct from the delayed FILLED (cum 3).
func TestWSUserFillReportDelay(t *testing.T) {
	const delay = 200 * time.Millisecond
	h := newWSHarness(t, WithFillDelay(func() time.Duration { return delay }))
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "user",
		"api_key":     testAPIKey,
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")

	// A resting LIMIT BUY at 100 with no crossing liquidity → prompt OPEN.
	body := `{"client_order_id":"cli-1","product_id":"BTC-USD","side":"BUY",` +
		`"order_configuration":{"limit_limit_gtc":{"base_size":"3","limit_price":"100"}}}`
	openStart := time.Now()
	resp := h.signedDoWS(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	env := wsReadChannel(t, c, "user")
	var open userEvent
	if err := json.Unmarshal(env.Events[0], &open); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if len(open.Orders) == 0 || open.Orders[0].Status != statusOpen {
		t.Fatalf("first user frame = %s, want OPEN", env.Events[0])
	}
	if dt := time.Since(openStart); dt > delay {
		t.Errorf("OPEN update was delayed (%v); only fill updates should be", dt)
	}

	// Fill the resting order with a book taker → fill update is delayed.
	fillStart := time.Now()
	wsAddOrder(t, h.book, "taker-sell", orderbook.SideSell, "100", "3")
	env = wsReadChannel(t, c, "user")
	var filled userEvent
	if err := json.Unmarshal(env.Events[0], &filled); err != nil {
		t.Fatalf("decode fill: %v", err)
	}
	if len(filled.Orders) == 0 || filled.Orders[0].Status != statusFilled {
		t.Fatalf("fill frame = %s, want FILLED", env.Events[0])
	}
	if filled.Orders[0].CumulativeQuantity != "3.00000000" {
		t.Errorf("cum qty = %s, want 3", filled.Orders[0].CumulativeQuantity)
	}
	if dt := time.Since(fillStart); dt < delay {
		t.Errorf("fill update arrived after %v, want >= %v (not delayed)", dt, delay)
	}
}

func TestWSUserUnauthenticatedRejected(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "user",
		"product_ids": []string{"BTC-USD"},
	})
	var ef errorFrame
	wsReadFrame(t, c, &ef)
	if ef.Type != "error" || ef.Channel != "user" {
		t.Fatalf("error frame = %+v", ef)
	}
	// Conn must stay open: a follow-up market_trades subscribe still works.
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")
}

func TestWSUserAuthenticatedFill(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "user",
		"api_key":     testAPIKey,
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")

	// Seed a resting ask so a market BUY via REST fills immediately.
	wsAddOrder(t, h.book, "seed-ask", orderbook.SideSell, "101", "5")

	body := `{"client_order_id":"cli-1","product_id":"BTC-USD","side":"BUY",` +
		`"order_configuration":{"market_market_ioc":{"base_size":"3"}}}`
	resp := h.signedDoWS(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Expect at least one user frame reporting the FILLED order with cumulative
	// quantity 3. There may be intermediate fill frames; scan until FILLED.
	deadline := time.Now().Add(3 * time.Second)
	for {
		env := wsReadChannel(t, c, "user")
		var ev userEvent
		if err := json.Unmarshal(env.Events[0], &ev); err != nil {
			t.Fatalf("decode user event: %v", err)
		}
		if len(ev.Orders) == 0 {
			t.Fatalf("user event has no orders: %s", env.Events[0])
		}
		o := ev.Orders[0]
		if o.ProductID != "BTC-USD" || o.Side != "BUY" {
			t.Fatalf("user order = %+v", o)
		}
		if o.Status == statusFilled {
			if o.CumulativeQuantity != "3.00000000" {
				t.Fatalf("cumulative_quantity = %s want 3", o.CumulativeQuantity)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw FILLED user frame")
		}
	}
}

func TestWSUnsubscribeStopsFrames(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")

	wsSend(t, c, map[string]interface{}{
		"type":        "unsubscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")
	// Wait until the unsubscribe has actually taken effect on the hub before
	// triggering a trade (the read pump processes it asynchronously).
	deadline := time.Now().Add(2 * time.Second)
	for h.csrv.broadcaster.hasMarketSubscribers(chanMarketTrades, "BTC-USD") {
		if time.Now().After(deadline) {
			t.Fatal("unsubscribe never took effect")
		}
		time.Sleep(5 * time.Millisecond)
	}

	wsAddOrder(t, h.book, "resting-bid", orderbook.SideBuy, "100", "5")
	wsAddOrder(t, h.book, "taker-sell", orderbook.SideSell, "100", "2")

	// No market_trades frame should arrive now.
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break // deadline -> good, no frame delivered
		}
		var env wsRawEnvelope
		_ = json.Unmarshal(data, &env)
		if env.Channel == "market_trades" {
			t.Fatal("received market_trades after unsubscribe")
		}
	}
}

func TestWSUnknownChannelAndProduct(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)

	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "bogus",
		"product_ids": []string{"BTC-USD"},
	})
	var ef errorFrame
	wsReadFrame(t, c, &ef)
	if ef.Type != "error" {
		t.Fatalf("expected error frame, got %+v", ef)
	}

	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "level2",
		"product_ids": []string{"DOGE-USD"},
	})
	var ef2 errorFrame
	wsReadFrame(t, c, &ef2)
	if ef2.Type != "error" || !strings.Contains(ef2.Message, "DOGE-USD") {
		t.Fatalf("expected unknown-product error, got %+v", ef2)
	}
}

func TestWSDisconnectCleansUp(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")

	// Close the client; the server read pump should detect it and unsubscribe.
	_ = c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for h.csrv.broadcaster.hasMarketSubscribers(chanMarketTrades, "BTC-USD") {
		if time.Now().After(deadline) {
			t.Fatal("disconnect did not clean up subscription")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The book hook must complete without blocking on the stale conn.
	done := make(chan struct{})
	go func() {
		wsAddOrder(t, h.book, "resting-bid", orderbook.SideBuy, "100", "5")
		wsAddOrder(t, h.book, "taker-sell", orderbook.SideSell, "100", "2")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("book hook blocked on a disconnected WS consumer")
	}
}

func TestWSSlowConsumerDropped(t *testing.T) {
	h := newWSHarness(t)
	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "market_trades",
		"product_ids": []string{"BTC-USD"},
	})
	_ = wsReadChannel(t, c, "subscriptions")

	// Never read from c again. Fire far more trades than the send buffer can
	// hold; the book hook (running under the book mutex) must never block on
	// the stalled socket. This is the load-bearing non-blocking guarantee.
	// (Whether the conn is then dropped depends on OS socket buffering, so we
	// assert the hook's non-blocking property, which is what protects the
	// engine.)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4*sendBuffer; i++ {
			wsAddOrder(t, h.book, "bid", orderbook.SideBuy, "100", "1")
			wsAddOrder(t, h.book, "sell", orderbook.SideSell, "100", "1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("book hook blocked on a slow WS consumer")
	}
}

// signedDoWS issues a SIGNED REST request from the WS harness (mirrors the REST
// harness's signedDo).
func (h *wsHarness) signedDoWS(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	tsStr := strconv.FormatInt(h.tsSec, 10)
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
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
