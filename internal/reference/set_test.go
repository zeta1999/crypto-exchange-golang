package reference

import (
	"context"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/replay"
)

func TestSetRoutesByInstrument(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	s := NewSet()
	s.Apply(feed.Event{Kind: feed.EventBook, Book: snap(1, t0, []feed.LOBLevel{lvl(100, 1)}, []feed.LOBLevel{lvl(101, 1)})})
	s.Apply(feed.Event{Kind: feed.EventBook, Book: &feed.LOBSnapshot{Instrument: "ETH-USD", Exchange: "coinbase", Timestamp: t0, Snapshot: true, Bids: []feed.LOBLevel{lvl(50, 1)}, Asks: []feed.LOBLevel{lvl(51, 1)}}})
	// A trade must not create a book.
	s.Apply(feed.Event{Kind: feed.EventTrade, Trade: &feed.Trade{Instrument: "DOGE-USD"}})

	if got := s.Instruments(); len(got) != 2 || got[0] != "BTC-USD" || got[1] != "ETH-USD" {
		t.Fatalf("instruments = %v", got)
	}
	if _, ok := s.Get("DOGE-USD"); ok {
		t.Error("trade should not have created a book")
	}
	eth, ok := s.Get("ETH-USD")
	if !ok {
		t.Fatal("ETH-USD book missing")
	}
	if bid, _ := eth.BestBid(); !bid.Price.Eq(dec("50")) {
		t.Errorf("ETH best bid = %v", bid.Price)
	}
}

// TestConsumeReplaySample drives the Set from the committed replay fixture
// and asserts the resulting deterministic book state. The fixture snapshots
// a BTC-USD book then removes its top bid via a diff.
func TestConsumeReplaySample(t *testing.T) {
	src := replay.New("../../testdata/feed/sample.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("replay start: %v", err)
	}

	s := NewSet()
	s.Consume(ctx, ch) // returns when the finite replay closes the channel

	// Only the book instrument is tracked; the binance trade line creates no book.
	if got := s.Instruments(); len(got) != 1 || got[0] != "BTC-USD" {
		t.Fatalf("instruments = %v, want [BTC-USD]", got)
	}
	b, _ := s.Get("BTC-USD")
	bid, ask, ok := b.BestBidAsk()
	if !ok {
		t.Fatal("book has no touch")
	}
	// Snapshot bids were 41999 and 41998.5; the diff removed 41999.
	if !bid.Price.Eq(dec("41998.5")) {
		t.Errorf("best bid = %v, want 41998.5 (41999 removed by diff)", bid.Price)
	}
	if !ask.Price.Eq(dec("42001")) {
		t.Errorf("best ask = %v, want 42001", ask.Price)
	}
	if mid, _ := b.Mid(); !mid.Eq(dec("41999.75")) {
		t.Errorf("mid = %v, want 41999.75", mid)
	}
	bids, asks := b.Depth(0)
	if len(bids) != 1 || len(asks) != 2 {
		t.Errorf("depth = %d bids / %d asks, want 1/2", len(bids), len(asks))
	}
}
