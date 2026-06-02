package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// Engine is the subset of the matching engine the Binance edge consumes.
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
}

// Server is a Binance-spot-compatible REST edge. It is an http.Handler that
// routes the api/v3 subset to the engine.
type Server struct {
	engine   Engine
	symbols  *SymbolMap
	auth     *Authenticator
	registry *Registry
	mux      *http.ServeMux
	now      func() time.Time
	balances []Balance
}

// Balance is a Binance account balance entry. The engine has no ledger, so the
// account endpoint returns a configured (possibly empty) static list — this is
// a documented stub.
type Balance struct {
	Asset  string `json:"asset"`
	Free   string `json:"free"`
	Locked string `json:"locked"`
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

// WithBalances sets the static balances returned by GET /api/v3/account.
func WithBalances(b []Balance) Option {
	return func(s *Server) { s.balances = b }
}

// New constructs the Binance edge. The caller is responsible for wiring the
// registry's OnTrade/OnCancel hooks onto the order book (see AttachHooks).
func New(engine Engine, symbols *SymbolMap, auth *Authenticator, registry *Registry, opts ...Option) *Server {
	s := &Server{
		engine:   engine,
		symbols:  symbols,
		auth:     auth,
		registry: registry,
		mux:      http.NewServeMux(),
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}

	// Public (unsigned) endpoints.
	s.mux.HandleFunc("/api/v3/ping", s.handlePing)
	s.mux.HandleFunc("/api/v3/time", s.handleTime)
	s.mux.HandleFunc("/api/v3/depth", s.handleDepth)
	s.mux.HandleFunc("/api/v3/ticker/price", s.handleTickerPrice)

	// SIGNED endpoints. /api/v3/order multiplexes POST (place) and DELETE
	// (cancel); openOrders and account are GET.
	s.mux.HandleFunc("/api/v3/order", s.handleOrder)
	s.mux.HandleFunc("/api/v3/openOrders", s.handleOpenOrders)
	s.mux.HandleFunc("/api/v3/account", s.handleAccount)

	return s
}

// Handler exposes the mux for httptest servers.
func (s *Server) Handler() http.Handler { return s.mux }

// AttachHooks registers the registry's trade/cancel hooks on an order book. A
// caller that constructs the book wires this once at startup.
func (s *Server) AttachHooks(book *orderbook.OrderBook) {
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
}

// ListenAndServe runs the edge with graceful shutdown, mirroring httpserver.
func ListenAndServe(ctx context.Context, addr string, srv *Server, certFile, keyFile string) error {
	server := &http.Server{Addr: addr, Handler: srv.mux}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if certFile != "" && keyFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	return server.ListenAndServe()
}

// writeError renders a Binance error body with its HTTP status.
func writeError(w http.ResponseWriter, err error) {
	ae, ok := err.(*apiError)
	if !ok {
		ae = errInternal(err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.HTTPStatus())
	_ = json.NewEncoder(w).Encode(ae)
}

// writeJSON renders a 200 JSON body.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		writeError(w, errInternal(err.Error()))
	}
}
