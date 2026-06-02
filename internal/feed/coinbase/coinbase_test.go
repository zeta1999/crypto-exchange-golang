package coinbase

import (
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

func TestParseTrades(t *testing.T) {
	raw := []byte(`{"channel":"market_trades","timestamp":"2026-06-02T00:00:00Z","sequence_num":5,"events":[{"type":"update","trades":[{"trade_id":"99","product_id":"BTC-USD","price":"42000.5","size":"0.01","side":"BUY","time":"2026-06-02T00:00:00.123Z"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(events) != 1 || events[0].Kind != feed.EventTrade {
		t.Fatalf("want 1 trade event, got %+v", events)
	}
	tr := events[0].Trade
	if tr.Instrument != "BTC-USD" || tr.Exchange != "coinbase" {
		t.Errorf("instrument/exchange = %q/%q", tr.Instrument, tr.Exchange)
	}
	if tr.Price != 42000.5 || tr.Quantity != 0.01 {
		t.Errorf("price/qty = %v/%v", tr.Price, tr.Quantity)
	}
	if tr.Side != "buy" {
		t.Errorf("side = %q, want buy (lowercased)", tr.Side)
	}
	if tr.TradeID != "99" {
		t.Errorf("trade id = %q", tr.TradeID)
	}
	if tr.Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}
}

func TestParseL2Snapshot(t *testing.T) {
	raw := []byte(`{"channel":"l2_data","timestamp":"2026-06-02T00:00:00Z","sequence_num":0,"events":[{"type":"snapshot","product_id":"BTC-USD","updates":[{"side":"bid","event_time":"2026-06-02T00:00:00Z","price_level":"41999.0","new_quantity":"1.2"},{"side":"offer","event_time":"2026-06-02T00:00:01Z","price_level":"42001.0","new_quantity":"0.5"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(events) != 1 || events[0].Kind != feed.EventBook {
		t.Fatalf("want 1 book event, got %+v", events)
	}
	b := events[0].Book
	if !b.Snapshot {
		t.Error("type=snapshot should set Snapshot=true")
	}
	if b.SequenceNumber != 0 {
		t.Errorf("seq = %d", b.SequenceNumber)
	}
	if len(b.Bids) != 1 || len(b.Asks) != 1 {
		t.Fatalf("levels = %dxb/%dxa", len(b.Bids), len(b.Asks))
	}
	if b.Bids[0].Price != 41999.0 || b.Bids[0].Quantity != 1.2 {
		t.Errorf("bid[0] = %+v", b.Bids[0])
	}
	if b.Asks[0].Price != 42001.0 || b.Asks[0].Quantity != 0.5 {
		t.Errorf("ask[0] = %+v", b.Asks[0])
	}
	// Timestamp should track the latest update event_time.
	if b.Timestamp.IsZero() {
		t.Error("timestamp not derived from updates")
	}
}

func TestParseL2UpdateRemoval(t *testing.T) {
	// new_quantity "0" is a level removal; surfaces as Quantity 0 on a diff.
	raw := []byte(`{"channel":"l2_data","sequence_num":7,"events":[{"type":"update","product_id":"BTC-USD","updates":[{"side":"bid","event_time":"2026-06-02T00:00:02Z","price_level":"41999.0","new_quantity":"0"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	b := events[0].Book
	if b.Snapshot {
		t.Error("type=update should set Snapshot=false")
	}
	if b.SequenceNumber != 7 {
		t.Errorf("seq = %d", b.SequenceNumber)
	}
	if len(b.Bids) != 1 || b.Bids[0].Quantity != 0 {
		t.Errorf("expected one bid removal, got %+v", b.Bids)
	}
	if b.Bids[0].QuantityDecimal != "0" {
		t.Errorf("removal decimal = %q", b.Bids[0].QuantityDecimal)
	}
}

func TestParseMessageIgnoresOther(t *testing.T) {
	// Subscriptions/heartbeats: known JSON, channel we don't map => no events.
	events, err := ParseMessage([]byte(`{"channel":"subscriptions","events":[]}`))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want 0 events, got %d", len(events))
	}
	if _, err := ParseMessage([]byte(`}{`)); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestParseL2SkipsUnparseableLevel(t *testing.T) {
	// Bad price_level / new_quantity must be dropped, not emitted as a zero
	// (a zero quantity reads as a level removal downstream).
	raw := []byte(`{"channel":"l2_data","sequence_num":1,"events":[{"type":"update","product_id":"BTC-USD","updates":[{"side":"bid","price_level":"oops","new_quantity":"1"},{"side":"bid","price_level":"41999.0","new_quantity":"2"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	b := events[0].Book
	if len(b.Bids) != 1 || b.Bids[0].Price != 41999.0 {
		t.Fatalf("want 1 valid bid (bad dropped), got %+v", b.Bids)
	}
}

func TestParseL2FallsBackToEnvelopeTime(t *testing.T) {
	// No per-update event_time => book timestamp falls back to the frame's
	// server timestamp rather than staying zero.
	raw := []byte(`{"channel":"l2_data","timestamp":"2026-06-02T01:02:03Z","sequence_num":1,"events":[{"type":"update","product_id":"BTC-USD","updates":[{"side":"bid","price_level":"1","new_quantity":"1"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	b := events[0].Book
	if b.Timestamp.IsZero() {
		t.Fatal("book timestamp should fall back to envelope time")
	}
	if got := b.Timestamp.UTC().Format(time.RFC3339); got != "2026-06-02T01:02:03Z" {
		t.Errorf("ts = %s", got)
	}
}

func TestParseTradesSkipsBadNumbers(t *testing.T) {
	raw := []byte(`{"channel":"market_trades","events":[{"type":"update","trades":[{"trade_id":"1","product_id":"BTC-USD","price":"x","size":"1","side":"BUY","time":"2026-06-02T00:00:00Z"}]}]}`)
	events, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want bad trade dropped, got %d events", len(events))
	}
}

func TestChannels(t *testing.T) {
	s := New(Config{Symbols: []string{"BTC-USD"}, FeedTypes: []string{"orderbook", "trades"}})
	got := s.channels()
	want := []string{"level2", "market_trades"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("channels = %v, want %v", got, want)
	}
}
