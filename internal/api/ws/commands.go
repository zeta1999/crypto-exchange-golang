package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Engine mirrors the HTTP/gRPC contract.
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
}

// Handler upgrades HTTP connections and processes JSON commands.
type Handler struct {
	engine    Engine
	validator *auth.TokenValidator
	upgrader  websocket.Upgrader

	cmdCount *metrics.CounterVec // labels: command, status
	cmdLat   *metrics.Histogram  // per-command processing latency (seconds)
}

// Instrument registers native-WS request metrics on reg.
func (h *Handler) Instrument(reg *metrics.Registry) {
	h.cmdCount = reg.NewCounterVec("exchange_ws_commands_total", "Native WS commands by command and status", "command", "status")
	h.cmdLat = reg.NewHistogram("exchange_ws_command_duration_seconds", "Native WS command processing latency", nil)
}

// NewHandler returns a websocket command processor.
func NewHandler(engine Engine, validator *auth.TokenValidator) *Handler {
	return &Handler{
		engine:    engine,
		validator: validator,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		h.handleOne(r.Context(), conn, data)
	}
}

// handleOne processes a single WS command, recording its command + status and
// latency into the request metrics (when instrumented).
func (h *Handler) handleOne(ctx context.Context, conn *websocket.Conn, data []byte) {
	start := time.Now()
	cmd, status := "unknown", "ok"
	defer func() {
		if h.cmdCount != nil {
			h.cmdCount.WithLabelValues(cmd, status).Inc()
			h.cmdLat.Observe(time.Since(start).Seconds())
		}
	}()

	var msg command
	if err := json.Unmarshal(data, &msg); err != nil {
		status = "invalid"
		_ = conn.WriteJSON(errorPayload{Error: "invalid_command"})
		return
	}
	cmd = boundCommand(msg.Command) // bounded label: never raw user input
	if err := h.validator.Validate(msg.Token); err != nil {
		status = "unauthorized"
		_ = conn.WriteJSON(errorPayload{Error: "unauthorized"})
		return
	}
	switch msg.Command {
	case "snapshot":
		snap, err := h.engine.Snapshot(msg.Instrument)
		if err != nil {
			status = "error"
			_ = conn.WriteJSON(errorPayload{Error: err.Error()})
			return
		}
		_ = conn.WriteJSON(snap)
	case "limit_order":
		ord := &orderbook.Order{
			ID:         msg.ClientID,
			Instrument: msg.Instrument,
			Price:      decimal.FromFloat(msg.Price),
			Volume:     decimal.FromFloat(msg.Volume),
			Side:       orderbook.Side(msg.Side),
		}
		trades, snap, err := h.engine.PlaceLimit(ctx, ord)
		if err != nil {
			status = "error"
			_ = conn.WriteJSON(errorPayload{Error: err.Error()})
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"trades": trades, "snapshot": snap})
	case "market_order":
		ord := &orderbook.Order{
			ID:         msg.ClientID,
			Instrument: msg.Instrument,
			Volume:     decimal.FromFloat(msg.Volume),
			Side:       orderbook.Side(msg.Side),
			IsMarket:   true,
		}
		trades, snap, err := h.engine.PlaceMarket(ctx, ord)
		if err != nil {
			status = "error"
			_ = conn.WriteJSON(errorPayload{Error: err.Error()})
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"trades": trades, "snapshot": snap})
	default:
		status = "unknown"
		_ = conn.WriteJSON(errorPayload{Error: "unknown_command"})
	}
}

// boundCommand maps a (user-supplied) command name to a fixed, low-cardinality
// metric label so an authenticated client cannot blow up the label space by
// sending arbitrary command strings.
func boundCommand(c string) string {
	switch c {
	case "snapshot", "limit_order", "market_order":
		return c
	default:
		return "other"
	}
}

type command struct {
	Command    string  `json:"command"`
	Instrument string  `json:"instrument"`
	Price      float64 `json:"price"`
	Volume     float64 `json:"volume"`
	Side       string  `json:"side"`
	ClientID   string  `json:"client_id"`
	Token      string  `json:"token"`
}

type errorPayload struct {
	Error string `json:"error"`
}

// DialExample shows how to connect without needing the GUI.
func DialExample(addr, token string, connFactory func(urlStr string) (*websocket.Conn, *http.Response, error)) error {
	conn, _, err := connFactory(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	payload := command{Command: "snapshot", Instrument: "BTC-USD", Token: token}
	if err := conn.WriteJSON(payload); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	return err
}
