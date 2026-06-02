package emulator

import (
	"context"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func tapeTrade(side string, price, qty float64) *feed.Trade {
	return &feed.Trade{
		Instrument:      "BTC-USD",
		Exchange:        "coinbase",
		Side:            side,
		Price:           price,
		Quantity:        qty,
		PriceDecimal:    decimal.FromFloat(price).String(),
		QuantityDecimal: decimal.FromFloat(qty).String(),
	}
}

func userLimit(id string, side orderbook.Side, price, qty float64) *orderbook.Order {
	return &orderbook.Order{
		ID:         id,
		Instrument: "BTC-USD",
		Side:       side,
		Price:      decimal.FromFloat(price),
		Volume:     decimal.FromFloat(qty),
	}
}

// TestTapeFillsRestingUserBuy is the Phase 5 DoD: a user buy limit resting at
// 100 is filled when the tape prints a sell (aggressor) through 100.
func TestTapeFillsRestingUserBuy(t *testing.T) {
	eng := newEngine("BTC-USD")
	if _, _, err := eng.PlaceLimit(context.Background(), userLimit("u1", orderbook.SideBuy, 100, 2)); err != nil {
		t.Fatal(err)
	}
	tape := NewTapeReplay(eng, "BTC-USD")

	// Tape sell of size 5 at price 99 (aggressor sold down through 100).
	trades, err := tape.Inject(context.Background(), tapeTrade("sell", 99, 5))
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("want 1 fill, got %d", len(trades))
	}
	// User bid fills at its own resting price (100), size 2 (its full volume).
	if !trades[0].Price.Eq(decimal.FromFloat(100)) || !trades[0].Volume.Eq(decimal.FromFloat(2)) {
		t.Errorf("fill = %v @ %v, want 2 @ 100", trades[0].Volume, trades[0].Price)
	}
	if trades[0].BuyOrderID != "u1" {
		t.Errorf("buyer = %q, want u1", trades[0].BuyOrderID)
	}
	// Book is empty afterward: user order fully filled, tape remainder cancelled.
	snap, _ := eng.Snapshot("BTC-USD")
	if len(snap.Bids) != 0 || len(snap.Asks) != 0 {
		t.Errorf("book not empty: %d bids / %d asks", len(snap.Bids), len(snap.Asks))
	}
}

// TestTapeFillsRestingUserSell mirrors the above for a buy-aggressor print.
func TestTapeFillsRestingUserSell(t *testing.T) {
	eng := newEngine("BTC-USD")
	if _, _, err := eng.PlaceLimit(context.Background(), userLimit("s1", orderbook.SideSell, 101, 3)); err != nil {
		t.Fatal(err)
	}
	tape := NewTapeReplay(eng, "BTC-USD")
	trades, err := tape.Inject(context.Background(), tapeTrade("buy", 102, 1))
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(trades) != 1 || !trades[0].Volume.Eq(decimal.FromFloat(1)) || !trades[0].Price.Eq(decimal.FromFloat(101)) {
		t.Fatalf("fill = %+v, want 1 @ 101", trades)
	}
	if trades[0].SellOrderID != "s1" {
		t.Errorf("seller = %q, want s1", trades[0].SellOrderID)
	}
	// The user sell had 3; tape took 1; 2 remain resting (tape order doesn't rest).
	snap, _ := eng.Snapshot("BTC-USD")
	if len(snap.Asks) != 1 || !snap.Asks[0].Volume.Eq(decimal.FromFloat(2)) {
		t.Errorf("remaining ask = %+v, want 2 @ 101", snap.Asks)
	}
	if len(snap.Bids) != 0 {
		t.Errorf("tape buy order should not rest: %d bids", len(snap.Bids))
	}
}

// TestTapeDoesNotFillBeyondPrice: a tape print must not fill a resting order
// priced worse than the tape price (capped at the print price).
func TestTapeDoesNotFillBeyondPrice(t *testing.T) {
	eng := newEngine("BTC-USD")
	// User sell at 105; a tape buy at 102 must NOT reach it.
	if _, _, err := eng.PlaceLimit(context.Background(), userLimit("s1", orderbook.SideSell, 105, 1)); err != nil {
		t.Fatal(err)
	}
	tape := NewTapeReplay(eng, "BTC-USD")
	trades, err := tape.Inject(context.Background(), tapeTrade("buy", 102, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 0 {
		t.Errorf("tape at 102 should not fill an ask at 105: %+v", trades)
	}
	snap, _ := eng.Snapshot("BTC-USD")
	if len(snap.Asks) != 1 {
		t.Errorf("user sell should remain: %+v", snap.Asks)
	}
}

// TestTapeIgnoresOtherInstrumentAndZero exercises the guards.
func TestTapeIgnoresOtherInstrumentAndZero(t *testing.T) {
	eng := newEngine("BTC-USD")
	tape := NewTapeReplay(eng, "BTC-USD")
	if tr, _ := tape.Inject(context.Background(), &feed.Trade{Instrument: "ETH-USD", Side: "buy", Price: 1, Quantity: 1}); len(tr) != 0 {
		t.Error("other instrument should be ignored")
	}
	if tr, _ := tape.Inject(context.Background(), tapeTrade("buy", 100, 0)); len(tr) != 0 {
		t.Error("zero-size trade should be ignored")
	}
}

// TestTapeRunConsumesTrades drives Run over a channel and checks a user order fills.
func TestTapeRunConsumesTrades(t *testing.T) {
	eng := newEngine("BTC-USD")
	if _, _, err := eng.PlaceLimit(context.Background(), userLimit("u1", orderbook.SideBuy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	tape := NewTapeReplay(eng, "BTC-USD")
	ch := make(chan feed.Event, 2)
	ch <- feed.Event{Kind: feed.EventBook} // ignored
	ch <- feed.Event{Kind: feed.EventTrade, Trade: tapeTrade("sell", 100, 1)}
	close(ch)
	tape.Run(context.Background(), ch)
	snap, _ := eng.Snapshot("BTC-USD")
	if len(snap.Bids) != 0 {
		t.Errorf("user buy should have filled via Run: %+v", snap.Bids)
	}
}
