package binance

import (
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func newTestRegistry() *Registry {
	return NewRegistry(func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() })
}

func TestRegistry_RecordAndGet(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTCUSDT", "BTC-USD", "BUY", "LIMIT", "GTC",
		decimal.MustParse("100"), decimal.MustParse("2"), "my-client-id")
	if rec.OrderID != 1 {
		t.Fatalf("OrderID = %d want 1", rec.OrderID)
	}
	if rec.EngineID != "binance:1" {
		t.Fatalf("EngineID = %q want binance:1", rec.EngineID)
	}
	if rec.Status != statusNew {
		t.Fatalf("Status = %q want NEW", rec.Status)
	}
	got, ok := reg.GetByClientOrderID("my-client-id")
	if !ok || got.OrderID != 1 {
		t.Fatalf("GetByClientOrderID failed: %v,%v", got, ok)
	}
}

func TestRegistry_PartialThenFullFill(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTCUSDT", "BTC-USD", "BUY", "LIMIT", "GTC",
		decimal.MustParse("100"), decimal.MustParse("2"), "")

	// Partial fill: 1 of 2.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: rec.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusPartiallyFilled {
		t.Fatalf("after partial: status = %q want PARTIALLY_FILLED", snap.Status)
	}
	if snap.ExecutedQty.Cmp(decimal.MustParse("1")) != 0 {
		t.Fatalf("executedQty = %s want 1", snap.ExecutedQty.StringPrec(2))
	}
	if snap.CummulativeQuoteQty.Cmp(decimal.MustParse("100")) != 0 {
		t.Fatalf("cummQuote = %s want 100", snap.CummulativeQuoteQty.StringPrec(2))
	}

	// Remaining fill: 1 more.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: rec.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	snap, _ = reg.snapshot(rec.OrderID)
	if snap.Status != statusFilled {
		t.Fatalf("after full: status = %q want FILLED", snap.Status)
	}
	if snap.ExecutedQty.Cmp(decimal.MustParse("2")) != 0 {
		t.Fatalf("executedQty = %s want 2", snap.ExecutedQty.StringPrec(2))
	}
}

func TestRegistry_CancelTransition(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTCUSDT", "BTC-USD", "SELL", "LIMIT", "GTC",
		decimal.MustParse("100"), decimal.MustParse("2"), "")
	reg.OnCancel(&orderbook.Order{ID: rec.EngineID})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusCanceled {
		t.Fatalf("status = %q want CANCELED", snap.Status)
	}
}

func TestRegistry_IgnoresSyntheticOrders(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTCUSDT", "BTC-USD", "BUY", "LIMIT", "GTC",
		decimal.MustParse("100"), decimal.MustParse("2"), "")
	// A trade between two synthetic orders must not touch our record.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: "seed:abc", SellOrderID: "tape:xyz",
		Instrument: "BTC-USD", Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusNew || snap.ExecutedQty.Sign() != 0 {
		t.Fatalf("synthetic trade leaked into record: %+v", snap)
	}
}

func TestRegistry_OpenOrdersFilter(t *testing.T) {
	reg := newTestRegistry()
	a := reg.Record("BTCUSDT", "BTC-USD", "BUY", "LIMIT", "GTC",
		decimal.MustParse("100"), decimal.MustParse("1"), "")
	reg.Record("ETHUSDT", "ETH-USD", "SELL", "LIMIT", "GTC",
		decimal.MustParse("50"), decimal.MustParse("1"), "")

	if all := reg.OpenOrders(""); len(all) != 2 {
		t.Fatalf("OpenOrders(all) = %d want 2", len(all))
	}
	btc := reg.OpenOrders("BTC-USD")
	if len(btc) != 1 || btc[0].OrderID != a.OrderID {
		t.Fatalf("OpenOrders(BTC-USD) = %v want only %d", btc, a.OrderID)
	}

	// Fully fill the BTC order: it drops out of open orders.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: a.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	if btc := reg.OpenOrders("BTC-USD"); len(btc) != 0 {
		t.Fatalf("after fill OpenOrders(BTC-USD) = %d want 0", len(btc))
	}
}
