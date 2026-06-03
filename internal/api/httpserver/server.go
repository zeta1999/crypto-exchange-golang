package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Engine matches the subset of the business logic consumed by HTTP.
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
}

// Server exposes JSON endpoints and static GUI assets.
// TransferFunc moves `amount` of `asset` from venue `from` to venue `to`
// (on-chain, via the transfer hub), returning the tx ref.
type TransferFunc func(ctx context.Context, from, to, asset string, amount decimal.Decimal) (string, error)

type Server struct {
	engine    Engine
	validator *auth.TokenValidator
	mux       *http.ServeMux
	transfer  TransferFunc // optional: POST /transfer (nil → 501)
}

// SetTransfer enables the native POST /transfer endpoint.
func (s *Server) SetTransfer(fn TransferFunc) { s.transfer = fn }

// Handler exposes the underlying mux for httptest servers.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// New wires handlers and static content. wsHandler is optional.
func New(engine Engine, validator *auth.TokenValidator, wsHandler http.Handler, uiFS http.FileSystem) *Server {
	mux := http.NewServeMux()
	srv := &Server{engine: engine, validator: validator, mux: mux}
	mux.HandleFunc("/orders", srv.handleOrders)
	mux.HandleFunc("/snapshot/", srv.handleSnapshot)
	mux.HandleFunc("/transfer", srv.handleTransfer)
	if wsHandler != nil {
		mux.Handle("/ws", wsHandler)
	}
	if uiFS != nil {
		mux.Handle("/", http.FileServer(uiFS))
	}
	return srv
}

// ListenAndServe starts the HTTP server with graceful shutdown.
func ListenAndServe(ctx context.Context, addr string, srv *Server, certFile, keyFile string) error {
	server := &http.Server{
		Addr:    addr,
		Handler: srv.mux,
	}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if certFile != "" && keyFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	return server.ListenAndServe()
}

type orderRequest struct {
	Instrument string  `json:"instrument"`
	Price      float64 `json:"price"`
	Volume     float64 `json:"volume"`
	Side       string  `json:"side"`
	Market     bool    `json:"market"`
	ClientID   string  `json:"client_id"`
	Token      string  `json:"token"`
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	token := tokenFromRequest(r)
	if token == "" {
		token = req.Token
	}
	if err := s.validator.Validate(token); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ord := &orderbook.Order{
		ID:         req.ClientID,
		Instrument: req.Instrument,
		Price:      decimal.FromFloat(req.Price),
		Volume:     decimal.FromFloat(req.Volume),
		Side:       orderbook.Side(req.Side),
		IsMarket:   req.Market,
	}
	var (
		trades []*orderbook.Trade
		snap   *orderbook.Snapshot
		err    error
	)
	if req.Market {
		trades, snap, err = s.engine.PlaceMarket(r.Context(), ord)
	} else {
		trades, snap, err = s.engine.PlaceLimit(r.Context(), ord)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, map[string]interface{}{
		"trades":   trades,
		"snapshot": snap,
	})
}

type transferRequest struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Asset  string `json:"asset"`
	Amount string `json:"amount"` // decimal string
	Token  string `json:"token"`
}

// handleTransfer: POST /transfer moves funds between venue accounts (on-chain).
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.transfer == nil {
		http.Error(w, "transfers not enabled", http.StatusNotImplemented)
		return
	}
	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	token := tokenFromRequest(r)
	if token == "" {
		token = req.Token
	}
	if err := s.validator.Validate(token); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.From == "" || req.To == "" || req.Asset == "" {
		http.Error(w, "from, to and asset are required", http.StatusBadRequest)
		return
	}
	amount, err := decimal.Parse(req.Amount)
	if err != nil {
		http.Error(w, "invalid amount", http.StatusBadRequest)
		return
	}
	ref, err := s.transfer(r.Context(), req.From, req.To, req.Asset, amount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, map[string]string{"tx_ref": ref})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	instrument := strings.TrimPrefix(r.URL.Path, "/snapshot/")
	if instrument == "" {
		http.Error(w, "instrument required", http.StatusBadRequest)
		return
	}
	if err := s.validator.Validate(tokenFromRequest(r)); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	snap, err := s.engine.Snapshot(instrument)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, snap)
}

func respondJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func tokenFromRequest(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		header = r.Header.Get("X-API-Token")
	}
	header = strings.TrimPrefix(header, "Bearer ")
	return strings.TrimSpace(header)
}
