package coinbase

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
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("2"), "my-client-id")
	if rec.OrderID != "emu-1" {
		t.Fatalf("OrderID = %q want emu-1", rec.OrderID)
	}
	if rec.EngineID != "coinbase:1" {
		t.Fatalf("EngineID = %q want coinbase:1", rec.EngineID)
	}
	if rec.Status != statusOpen {
		t.Fatalf("Status = %q want OPEN", rec.Status)
	}
	got, ok := reg.Get("emu-1")
	if !ok || got.ClientOrderID != "my-client-id" {
		t.Fatalf("Get failed: %v,%v", got, ok)
	}
}

func TestRegistry_GeneratedClientOrderID(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("1"), "")
	if rec.ClientOrderID == "" {
		t.Fatalf("expected a generated client_order_id")
	}
}

func TestRegistry_PartialThenFullFill(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("2"), "")

	// Partial fill: 1 of 2.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: rec.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusOpen {
		t.Fatalf("after partial: status = %q want OPEN", snap.Status)
	}
	if snap.FilledSize.Cmp(decimal.MustParse("1")) != 0 {
		t.Fatalf("filledSize = %s want 1", snap.FilledSize.StringPrec(2))
	}
	if snap.avgFilledPrice().Cmp(decimal.MustParse("100")) != 0 {
		t.Fatalf("avgFilledPrice = %s want 100", snap.avgFilledPrice().StringPrec(2))
	}

	// Remaining fill at a different price: 1 more @ 102 → avg = 101.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: rec.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("102"), Volume: decimal.MustParse("1"),
	})
	snap, _ = reg.snapshot(rec.OrderID)
	if snap.Status != statusFilled {
		t.Fatalf("after full: status = %q want FILLED", snap.Status)
	}
	if snap.FilledSize.Cmp(decimal.MustParse("2")) != 0 {
		t.Fatalf("filledSize = %s want 2", snap.FilledSize.StringPrec(2))
	}
	if snap.avgFilledPrice().Cmp(decimal.MustParse("101")) != 0 {
		t.Fatalf("avgFilledPrice = %s want 101", snap.avgFilledPrice().StringPrec(2))
	}
}

func TestRegistry_CancelTransition(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "SELL", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("2"), "")
	reg.OnCancel(&orderbook.Order{ID: rec.EngineID})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusCancelled {
		t.Fatalf("status = %q want CANCELLED", snap.Status)
	}
}

func TestRegistry_CancelDoesNotOverrideFilled(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("1"), "")
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: rec.EngineID, Instrument: "BTC-USD",
		Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	reg.OnCancel(&orderbook.Order{ID: rec.EngineID})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusFilled {
		t.Fatalf("status = %q want FILLED (cancel must not override)", snap.Status)
	}
}

func TestRegistry_IgnoresForeignOrders(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("2"), "")
	// A trade between synthetic + another edge's order must not touch our record.
	reg.OnTrade(&orderbook.Trade{
		BuyOrderID: "seed:abc", SellOrderID: "binance:7",
		Instrument: "BTC-USD", Price: decimal.MustParse("100"), Volume: decimal.MustParse("1"),
	})
	snap, _ := reg.snapshot(rec.OrderID)
	if snap.Status != statusOpen || snap.FilledSize.Sign() != 0 {
		t.Fatalf("foreign trade leaked into record: %+v", snap)
	}
}

func TestRegistry_RollbackOnPlacementError(t *testing.T) {
	reg := newTestRegistry()
	rec := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("2"), "")
	if _, ok := reg.Get(rec.OrderID); !ok {
		t.Fatal("record should exist before rollback")
	}
	reg.Remove(rec.OrderID)
	if _, ok := reg.Get(rec.OrderID); ok {
		t.Fatal("record should be gone after Remove")
	}
	if len(reg.OpenOrders("")) != 0 {
		t.Fatal("removed order must not appear in open orders")
	}
}

func TestRegistry_OpenOrdersFilter(t *testing.T) {
	reg := newTestRegistry()
	a := reg.Record("BTC-USD", "BUY", "LIMIT", false,
		decimal.MustParse("100"), decimal.MustParse("1"), "")
	reg.Record("ETH-USD", "SELL", "LIMIT", false,
		decimal.MustParse("50"), decimal.MustParse("1"), "")

	if all := reg.OpenOrders(""); len(all) != 2 {
		t.Fatalf("OpenOrders(all) = %d want 2", len(all))
	}
	btc := reg.OpenOrders("BTC-USD")
	if len(btc) != 1 || btc[0].OrderID != a.OrderID {
		t.Fatalf("OpenOrders(BTC-USD) = %v want only %s", btc, a.OrderID)
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

func TestProducts_ResolveAndList(t *testing.T) {
	p := NewProducts([]string{"BTC-USD", "ETH-USD", "BTC-USD", ""})
	if eng, ok := p.Resolve("BTC-USD"); !ok || eng != "BTC-USD" {
		t.Fatalf("Resolve(BTC-USD) = %q,%v", eng, ok)
	}
	if _, ok := p.Resolve("DOGE-USD"); ok {
		t.Fatalf("Resolve(DOGE-USD) should be false")
	}
	list := p.List()
	if len(list) != 2 || list[0] != "BTC-USD" || list[1] != "ETH-USD" {
		t.Fatalf("List() = %v want [BTC-USD ETH-USD]", list)
	}
}
