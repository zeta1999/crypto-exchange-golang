package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// fixedRecv is an arbitrary deterministic stamp for book frames in tests.
var fixedRecv = time.Unix(1700000001, 0).UTC()

func TestParseTrade(t *testing.T) {
	// m=false => buyer is the taker => buy aggressor.
	raw := []byte(`{"stream":"btcusdt@trade","data":{"e":"trade","s":"BTCUSDT","t":12345,"p":"42000.50","q":"0.030","T":1700000000000,"m":false}}`)
	events, err := ParseMessage(raw, fixedRecv)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(events) != 1 || events[0].Kind != feed.EventTrade {
		t.Fatalf("want 1 trade event, got %+v", events)
	}
	tr := events[0].Trade
	if tr.Instrument != "BTCUSDT" || tr.Exchange != "binance" {
		t.Errorf("instrument/exchange = %q/%q", tr.Instrument, tr.Exchange)
	}
	if tr.Price != 42000.50 || tr.Quantity != 0.030 {
		t.Errorf("price/qty = %v/%v", tr.Price, tr.Quantity)
	}
	if tr.Side != "buy" {
		t.Errorf("side = %q, want buy", tr.Side)
	}
	if tr.TradeID != "12345" {
		t.Errorf("trade id = %q", tr.TradeID)
	}
	if tr.PriceDecimal != "42000.50" || tr.QuantityDecimal != "0.030" {
		t.Errorf("decimals = %q/%q", tr.PriceDecimal, tr.QuantityDecimal)
	}
	if got := tr.Timestamp.UnixMilli(); got != 1700000000000 {
		t.Errorf("ts = %d", got)
	}
}

func TestParseTradeMakerSideIsSell(t *testing.T) {
	// m=true => buyer is the maker => sell aggressor.
	raw := []byte(`{"stream":"btcusdt@trade","data":{"s":"BTCUSDT","t":1,"p":"100","q":"1","T":1,"m":true}}`)
	events, err := ParseMessage(raw, fixedRecv)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if events[0].Trade.Side != "sell" {
		t.Errorf("side = %q, want sell", events[0].Trade.Side)
	}
}

func TestParseDepth(t *testing.T) {
	// depth20 carries no symbol field; it must come from the stream name.
	raw := []byte(`{"stream":"btcusdt@depth20@100ms","data":{"bids":[["42000.10","1.5"],["41999.00","2.0"]],"asks":[["42001.00","0.8"]]}}`)
	events, err := ParseMessage(raw, fixedRecv)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(events) != 1 || events[0].Kind != feed.EventBook {
		t.Fatalf("want 1 book event, got %+v", events)
	}
	b := events[0].Book
	if b.Instrument != "BTCUSDT" {
		t.Errorf("instrument = %q, want BTCUSDT", b.Instrument)
	}
	if !b.Snapshot {
		t.Error("depth20 should be a full snapshot")
	}
	if len(b.Bids) != 2 || len(b.Asks) != 1 {
		t.Fatalf("levels = %dxb/%dxa", len(b.Bids), len(b.Asks))
	}
	if b.Bids[0].Price != 42000.10 || b.Bids[0].Quantity != 1.5 {
		t.Errorf("bid[0] = %+v", b.Bids[0])
	}
	if b.Asks[0].PriceDecimal != "42001.00" {
		t.Errorf("ask[0] decimal = %q", b.Asks[0].PriceDecimal)
	}
}

func TestParseMessageIgnoresUnknownAndJunk(t *testing.T) {
	if _, err := ParseMessage([]byte(`not json`), fixedRecv); err == nil {
		t.Error("expected error on non-JSON")
	}
	// Known wrapper, unsubscribed stream type => no events, no error.
	events, err := ParseMessage([]byte(`{"stream":"btcusdt@kline_1m","data":{}}`), fixedRecv)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want 0 events, got %d", len(events))
	}
}

func TestParseDepthTimestampIsRecv(t *testing.T) {
	// Book frames carry no event time on the wire, so the recv stamp must
	// flow through deterministically (no time.Now() inside the parser).
	raw := []byte(`{"stream":"btcusdt@depth20@100ms","data":{"bids":[["1","2"]],"asks":[]}}`)
	events, err := ParseMessage(raw, fixedRecv)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if !events[0].Book.Timestamp.Equal(fixedRecv) {
		t.Errorf("book ts = %v, want %v", events[0].Book.Timestamp, fixedRecv)
	}
}

func TestParseDepthSkipsUnparseableLevel(t *testing.T) {
	// A level that fails to parse must be dropped, not emitted as price/qty
	// 0 (a zero quantity is the book's removal signal).
	raw := []byte(`{"stream":"btcusdt@depth20@100ms","data":{"bids":[["abc","1.0"],["42000.0","0.5"]],"asks":[]}}`)
	events, err := ParseMessage(raw, fixedRecv)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	b := events[0].Book
	if len(b.Bids) != 1 {
		t.Fatalf("want 1 valid bid (bad one dropped), got %d: %+v", len(b.Bids), b.Bids)
	}
	if b.Bids[0].Price != 42000.0 {
		t.Errorf("bid[0] = %+v", b.Bids[0])
	}
}

func TestParseTradeRejectsBadNumbers(t *testing.T) {
	raw := []byte(`{"stream":"btcusdt@trade","data":{"s":"BTCUSDT","t":1,"p":"notaprice","q":"1","T":1,"m":false}}`)
	if _, err := ParseMessage(raw, fixedRecv); err == nil {
		t.Error("expected error on unparseable trade price")
	}
}

// TestStartLifecycle stands up a fake websocket server, streams two frames,
// then cancels ctx and asserts the event channel drains and closes — i.e.
// the producer goroutine exits cleanly (no leak / no block on send).
func TestStartLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"stream":"btcusdt@trade","data":{"s":"BTCUSDT","t":1,"p":"100","q":"1","T":1,"m":false}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"stream":"btcusdt@depth20@100ms","data":{"bids":[["99","1"]],"asks":[["101","1"]]}}`))
		for { // block until the client disconnects on ctx cancel
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	s := New(Config{Symbols: []string{"BTCUSDT"}, FeedTypes: []string{"trades"}, WSURL: wsURL})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := make([]feed.Event, 0, 2)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d events", len(got))
		}
	}
	if got[0].Kind != feed.EventTrade || got[1].Kind != feed.EventBook {
		t.Errorf("event kinds = %v, %v", got[0].Kind, got[1].Kind)
	}

	cancel()
	// Channel must close once the goroutine observes cancellation.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if s.Status().State != "closed" {
					t.Errorf("state after close = %q, want closed", s.Status().State)
				}
				return
			}
		case <-deadline:
			t.Fatal("event channel not closed after ctx cancel")
		}
	}
}

func TestStreams(t *testing.T) {
	s := New(Config{Symbols: []string{"BTCUSDT", "ETHUSDT"}, FeedTypes: []string{"trades", "orderbook"}})
	got := s.streams()
	want := []string{"btcusdt@trade", "btcusdt@depth20@100ms", "ethusdt@trade", "ethusdt@depth20@100ms"}
	if len(got) != len(want) {
		t.Fatalf("streams = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("streams[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseTradeRejectsNonFinite(t *testing.T) {
	for _, raw := range []string{
		`{"stream":"btcusdt@trade","data":{"s":"BTCUSDT","t":1,"p":"NaN","q":"1","T":1,"m":false}}`,
		`{"stream":"btcusdt@trade","data":{"s":"BTCUSDT","t":1,"p":"Inf","q":"1","T":1,"m":false}}`,
	} {
		if _, err := ParseMessage([]byte(raw), fixedRecv); err == nil {
			t.Errorf("non-finite trade %q should be rejected", raw)
		}
	}
}
