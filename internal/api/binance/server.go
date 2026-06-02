package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
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
	engine      Engine
	symbols     *SymbolMap
	auth        *Authenticator
	registry    *Registry
	mux         *http.ServeMux
	now         func() time.Time
	balances    []Balance
	broadcaster *Broadcaster
	listenKeys  *listenKeyManager
	limiter     *ratelimit.KeyedLimiter
	metrics     *Metrics
	handler     http.Handler // mux wrapped with rate-limit + metrics middleware
	ackDelay    func() time.Duration
	fillDelay   func() time.Duration
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

// WithAckDelay injects an artificial order-acknowledgement latency: the
// place/cancel handlers sleep delay() before writing the response, simulating a
// slow venue edge (Phase 7). delay() may return jitter. nil/zero = no delay.
func WithAckDelay(delay func() time.Duration) Option {
	return func(s *Server) { s.ackDelay = delay }
}

// WithFillDelay injects an artificial fill-report latency: a fill (TRADE)
// executionReport is built at fill time (so its E/z/X fields are fill-time
// values) but its delivery to user-data subscribers is held back by delay(),
// simulating a venue whose order updates lag the actual execution (Phase 7).
// delay() may return jitter. nil/zero = no delay. Only fill reports are
// delayed; NEW/CANCELED updates deliver promptly.
//
// Caveat: delivery uses an independent timer per fill, so unlike the rest of
// the engine this path is NOT deterministic — under jitter (or bursts of
// near-simultaneous fills) reports may be DELIVERED OUT OF EXECUTION ORDER.
// Each frame still carries cumulative z/X, so treat a frame as the
// authoritative snapshot at its event time; do not reconstruct a ledger from
// the per-fill l/L across reordered frames. This models a reorder-on-the-wire
// venue, which is intentional for a test bed.
func WithFillDelay(delay func() time.Duration) Option {
	return func(s *Server) { s.fillDelay = delay }
}

// sleepAck applies the configured order-ack latency on the (HTTP handler)
// goroutine, respecting cancellation. Safe here — unlike the book-hook path, a
// handler goroutine may block without stalling the matching engine.
func (s *Server) sleepAck(ctx context.Context) {
	if s.ackDelay == nil {
		return
	}
	d := s.ackDelay()
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
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
	s.broadcaster = NewBroadcaster()
	s.listenKeys = newListenKeyManager(s.now)

	// Public (unsigned) endpoints.
	s.mux.HandleFunc("/api/v3/ping", s.handlePing)
	s.mux.HandleFunc("/api/v3/time", s.handleTime)
	s.mux.HandleFunc("/api/v3/exchangeInfo", s.handleExchangeInfo)
	s.mux.HandleFunc("/api/v3/depth", s.handleDepth)
	s.mux.HandleFunc("/api/v3/ticker/price", s.handleTickerPrice)

	// SIGNED endpoints. /api/v3/order multiplexes POST (place) and DELETE
	// (cancel); openOrders and account are GET.
	s.mux.HandleFunc("/api/v3/order", s.handleOrder)
	s.mux.HandleFunc("/api/v3/openOrders", s.handleOpenOrders)
	s.mux.HandleFunc("/api/v3/account", s.handleAccount)

	// listenKey lifecycle for the user-data stream (requires X-MBX-APIKEY, not a
	// signature — matching Binance).
	s.mux.HandleFunc("/api/v3/userDataStream", s.handleUserDataStream)

	// WebSocket streams: raw single stream / listenKey, and combined.
	s.mux.HandleFunc("/ws/", s.handleRawStream)
	s.mux.HandleFunc("/stream", s.handleCombinedStream)

	s.handler = s.middleware(s.mux)
	return s
}

// Handler exposes the (rate-limited, metered) handler for httptest servers.
func (s *Server) Handler() http.Handler { return s.handler }

// AttachHooks registers the edge's order-book hooks. TWO hooks are registered,
// and ORDER MATTERS: hooks fire in registration order (see orderbook.fire), so
// the registry's state-updating hook is registered FIRST and the WebSocket
// emission hook SECOND. The WS hook reads the registry snapshot for
// executionReport, so it must run after the registry has folded the
// trade/cancel into the record. A caller wires this once at startup.
func (s *Server) AttachHooks(book *orderbook.OrderBook) {
	// 1) Registry hook: update per-order fill/cancel state.
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

	// 2) WebSocket hook: broadcast market @trade events and, for Binance-edge
	// orders, user-data executionReport updates (reading the now-updated record).
	book.RegisterHook(func(evt string, data interface{}) {
		switch evt {
		case "trade":
			t, ok := data.(*orderbook.Trade)
			if !ok {
				return
			}
			s.onBookTrade(t) // public @trade market stream
			// executionReport for whichever side(s) belong to the Binance edge,
			// carrying THIS trade's fill qty/price (Binance l/L).
			for _, id := range []string{t.BuyOrderID, t.SellOrderID} {
				if isEdgeOrder(id) {
					s.emitExecutionReport(id, execTypeTrade, t.Volume, t.Price)
				}
			}
		case "cancel":
			if o, ok := data.(*orderbook.Order); ok && isEdgeOrder(o.ID) {
				s.emitExecutionReport(o.ID, execTypeCanceled, decimal.Zero, decimal.Zero)
			}
		}
	})
}

// isEdgeOrder reports whether an engine order id originates from the Binance
// edge (so synthetic emulator/tape/toxic order ids are ignored by WS emission).
func isEdgeOrder(id string) bool { return strings.HasPrefix(id, enginePrefix) }

// ListenAndServe runs the edge with graceful shutdown, mirroring httpserver. It
// also starts the WebSocket @depth push ticker on ctx.
func ListenAndServe(ctx context.Context, addr string, srv *Server, certFile, keyFile string) error {
	server := &http.Server{Addr: addr, Handler: srv.handler}
	go srv.Start(ctx) // periodic @depth20 broadcast loop
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
