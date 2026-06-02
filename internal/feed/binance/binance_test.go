package binance

import (
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

func TestParseTrade(t *testing.T) {
	// m=false => buyer is the taker => buy aggressor.
	raw := []byte(`{"stream":"btcusdt@trade","data":{"e":"trade","s":"BTCUSDT","t":12345,"p":"42000.50","q":"0.030","T":1700000000000,"m":false}}`)
	events, err := ParseMessage(raw)
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
	events, err := ParseMessage(raw)
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
	events, err := ParseMessage(raw)
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
	if _, err := ParseMessage([]byte(`not json`)); err == nil {
		t.Error("expected error on non-JSON")
	}
	// Known wrapper, unsubscribed stream type => no events, no error.
	events, err := ParseMessage([]byte(`{"stream":"btcusdt@kline_1m","data":{}}`))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want 0 events, got %d", len(events))
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
