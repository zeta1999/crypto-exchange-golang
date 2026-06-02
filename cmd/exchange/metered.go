package main

import (
	"context"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// meteredEngine wraps the engine for one API edge, counting order placements
// under the edge's label. It satisfies the binance/coinbase/httpserver Engine
// interfaces (which are a subset of *engine.Engine's methods). Counting here —
// rather than inside the engine — keeps the engine edge-agnostic and attributes
// each placement to the API that originated it.
type meteredEngine struct {
	inner   *engine.Engine
	placed  *metrics.Counter
	enabled bool
}

func newMeteredEngine(inner *engine.Engine, vec *metrics.CounterVec, edge string) *meteredEngine {
	m := &meteredEngine{inner: inner}
	if vec != nil {
		m.placed = vec.WithLabelValues(edge)
		m.enabled = true
	}
	return m
}

func (m *meteredEngine) inc() {
	if m.enabled {
		m.placed.Inc()
	}
}

func (m *meteredEngine) PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	m.inc()
	return m.inner.PlaceLimit(ctx, ord)
}

func (m *meteredEngine) PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	m.inc()
	return m.inner.PlaceMarket(ctx, ord)
}

func (m *meteredEngine) Snapshot(symbol string) (*orderbook.Snapshot, error) {
	return m.inner.Snapshot(symbol)
}

func (m *meteredEngine) CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error) {
	return m.inner.CancelOrder(ctx, instrument, orderID)
}
