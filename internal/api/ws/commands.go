package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
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
		var msg command
		if err := json.Unmarshal(data, &msg); err != nil {
			_ = conn.WriteJSON(errorPayload{Error: "invalid_command"})
			continue
		}
		if err := h.validator.Validate(msg.Token); err != nil {
			_ = conn.WriteJSON(errorPayload{Error: "unauthorized"})
			continue
		}
		switch msg.Command {
		case "snapshot":
			snap, err := h.engine.Snapshot(msg.Instrument)
			if err != nil {
				_ = conn.WriteJSON(errorPayload{Error: err.Error()})
				continue
			}
			_ = conn.WriteJSON(snap)
		case "limit_order":
			ord := &orderbook.Order{
				ID:         msg.ClientID,
				Instrument: msg.Instrument,
				Price:      msg.Price,
				Volume:     msg.Volume,
				Side:       orderbook.Side(msg.Side),
			}
			trades, snap, err := h.engine.PlaceLimit(r.Context(), ord)
			if err != nil {
				_ = conn.WriteJSON(errorPayload{Error: err.Error()})
				continue
			}
			_ = conn.WriteJSON(map[string]interface{}{"trades": trades, "snapshot": snap})
		case "market_order":
			ord := &orderbook.Order{
				ID:         msg.ClientID,
				Instrument: msg.Instrument,
				Volume:     msg.Volume,
				Side:       orderbook.Side(msg.Side),
				IsMarket:   true,
			}
			trades, snap, err := h.engine.PlaceMarket(r.Context(), ord)
			if err != nil {
				_ = conn.WriteJSON(errorPayload{Error: err.Error()})
				continue
			}
			_ = conn.WriteJSON(map[string]interface{}{"trades": trades, "snapshot": snap})
		default:
			_ = conn.WriteJSON(errorPayload{Error: "unknown_command"})
		}
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
