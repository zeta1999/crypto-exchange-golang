package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
)

// Engine is the subset of the matching engine the Coinbase edge consumes.
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
}

// Server is a Coinbase-Advanced-Trade-compatible REST edge. It is an
// http.Handler that routes the /api/v3/brokerage subset to the engine.
type Server struct {
	engine      Engine
	products    *Products
	auth        *Authenticator
	registry    *Registry
	mux         *http.ServeMux
	now         func() time.Time
	accounts    []Account
	broadcaster *Broadcaster
	tradeSeq    atomic.Int64
	limiter     *ratelimit.KeyedLimiter
	metrics     *Metrics
	handler     http.Handler // mux wrapped with rate-limit + metrics middleware
}

// nextTradeID synthesizes a monotonic trade_id for a market_trades frame (the
// order book Trade carries no id). Independent of REST order IDs.
func (s *Server) nextTradeID() string {
	return strconv.FormatInt(s.tradeSeq.Add(1), 10)
}

// Account is a Coinbase brokerage account entry. The engine has no ledger, so
// the accounts endpoint returns a configured (possibly empty) static list —
// this is a documented stub.
type Account struct {
	UUID             string `json:"uuid"`
	Name             string `json:"name"`
	Currency         string `json:"currency"`
	AvailableBalance Money  `json:"available_balance"`
	Hold             Money  `json:"hold"`
	Active           bool   `json:"active"`
	Type             string `json:"type"`
}

// Money is a Coinbase {value, currency} amount.
type Money struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// Option customises the Server.
type Option func(*Server)

// WithClock injects a clock (for deterministic tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// WithAccounts sets the static accounts returned by GET .../accounts.
func WithAccounts(a []Account) Option {
	return func(s *Server) { s.accounts = a }
}

// New constructs the Coinbase edge. The caller is responsible for wiring the
// registry's OnTrade/OnCancel hooks onto the order book (see AttachHooks).
func New(engine Engine, products *Products, auth *Authenticator, registry *Registry, opts ...Option) *Server {
	s := &Server{
		engine:      engine,
		products:    products,
		auth:        auth,
		registry:    registry,
		mux:         http.NewServeMux(),
		now:         time.Now,
		broadcaster: NewBroadcaster(),
	}
	for _, opt := range opts {
		opt(s)
	}

	// Public (unsigned) endpoints.
	s.mux.HandleFunc("/api/v3/brokerage/time", s.handleTime)
	s.mux.HandleFunc("/api/v3/brokerage/product_book", s.handleProductBook)
	s.mux.HandleFunc("/api/v3/brokerage/products/", s.handleProduct)

	// SIGNED endpoints.
	s.mux.HandleFunc("/api/v3/brokerage/orders", s.handleCreateOrder)
	s.mux.HandleFunc("/api/v3/brokerage/orders/batch_cancel", s.handleBatchCancel)
	s.mux.HandleFunc("/api/v3/brokerage/orders/historical/batch", s.handleHistoricalBatch)
	s.mux.HandleFunc("/api/v3/brokerage/orders/historical/", s.handleHistoricalOrder)
	s.mux.HandleFunc("/api/v3/brokerage/accounts", s.handleAccounts)

	// WebSocket: single message-driven endpoint (client sends subscribe frames).
	s.mux.HandleFunc("/ws", s.handleWS)

	s.handler = s.middleware(s.mux)
	return s
}

// Handler exposes the (rate-limited, metered) handler for httptest servers.
func (s *Server) Handler() http.Handler { return s.handler }

// AttachHooks registers the registry's trade/cancel hooks on an order book. A
// caller that constructs the book wires this once at startup.
func (s *Server) AttachHooks(book *orderbook.OrderBook) {
	// Registry hook FIRST: folds the trade/cancel into the order record.
	book.RegisterHook(func(evt string, data interface{}) {
		switch evt {
		case "trade":
			if t, ok := data.(*orderbook.Trade); ok {
				s.registry.OnTrade(t)
			}
		case "cancel":
			if o, ok := data.(*orderbook.Order); ok {
				s.registry.OnCancel(o)
			}
		}
	})
	// WS emit hook SECOND: reads the now-updated record to push market_trades
	// and user-channel frames. Registration order guarantees the record is
	// current when this fires.
	book.RegisterHook(func(evt string, data interface{}) {
		switch evt {
		case "trade":
			if t, ok := data.(*orderbook.Trade); ok {
				s.onBookTrade(t)
			}
		case "cancel":
			if o, ok := data.(*orderbook.Order); ok {
				s.onBookCancel(o)
			}
		}
	})
}

// ListenAndServe runs the edge with graceful shutdown, mirroring httpserver.
func ListenAndServe(ctx context.Context, addr string, srv *Server, certFile, keyFile string) error {
	server := &http.Server{Addr: addr, Handler: srv.handler}
	// Run the level2 refresh ticker for the lifetime of the server.
	go srv.Start(ctx)
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if certFile != "" && keyFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	return server.ListenAndServe()
}

// writeError renders a Coinbase error body with its HTTP status.
func writeError(w http.ResponseWriter, err error) {
	ae, ok := err.(*apiError)
	if !ok {
		ae = errInternalf(err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.HTTPStatus())
	_ = json.NewEncoder(w).Encode(ae)
}

// writeJSON renders a 200 JSON body.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		writeError(w, errInternalf(err.Error()))
	}
}
