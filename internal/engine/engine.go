package engine

import (
	"context"
	"fmt"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/wal"
)

// MarginValidator is a plug point to enforce customer credit policies.
type MarginValidator interface {
	Validate(ctx context.Context, ord *orderbook.Order) error
}

// Engine orchestrates order validation, routing, and hooks.
type Engine struct {
	book     *orderbook.OrderBook
	margin   MarginValidator
	recorder wal.Appender
}

// New instantiates the matching engine with an order book and margin checker.
func New(book *orderbook.OrderBook, margin MarginValidator, recorder wal.Appender) *Engine {
	if recorder == nil {
		recorder = wal.Nop()
	}
	return &Engine{book: book, margin: margin, recorder: recorder}
}

// PlaceLimit creates a limit order and returns executed trades with the latest snapshot.
func (e *Engine) PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	if err := e.margin.Validate(ctx, ord); err != nil {
		return nil, nil, fmt.Errorf("margin validation failed: %w", err)
	}
	_ = e.recorder.Append("order.limit", snapshotOrder(ord))
	trades, err := e.book.AddLimitOrder(ord)
	if err != nil {
		return nil, nil, err
	}
	snap, err := e.book.Snapshot(ord.Instrument)
	if err != nil {
		return trades, nil, err
	}
	return trades, snap, nil
}

// PlaceMarket creates a market order respecting margin rules.
func (e *Engine) PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	ord.IsMarket = true
	_ = e.recorder.Append("order.market", snapshotOrder(ord))
	return e.PlaceLimit(ctx, ord)
}

// Snapshot exposes public market data for dashboards.
func (e *Engine) Snapshot(symbol string) (*orderbook.Snapshot, error) {
	return e.book.Snapshot(symbol)
}

// CancelOrder removes a resting order and records the intent.
func (e *Engine) CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error) {
	_ = e.recorder.Append("order.cancel", map[string]string{"id": orderID, "instrument": instrument})
	return e.book.CancelOrder(instrument, orderID)
}

func snapshotOrder(ord *orderbook.Order) map[string]interface{} {
	if ord == nil {
		return nil
	}
	return map[string]interface{}{
		"id":         ord.ID,
		"instrument": ord.Instrument,
		"price":      ord.Price,
		"volume":     ord.Volume,
		"side":       ord.Side,
		"market":     ord.IsMarket,
	}
}
