// Package deribit publishes the shared options book as Deribit public market data.
// Market data only: GET /api/v2/public/ticker. No order entry.
package deribit

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zeta1999/crypto-exchange-golang/internal/api/optlisten"
	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// New serves the public ticker for the shared book.
func New(book *optmarket.Market) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/public/ticker", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("instrument_name")
		coin, strike, kind, expiry, err := optmarket.ParseDeribitName(name)
		if err != nil {
			writeErr(w, 400, "bad instrument_name")
			return
		}
		in, ok := book.Find(coin, strike, kind, expiry)
		if !ok {
			writeErr(w, 1001, "instrument not found")
			return
		}
		mk, err := book.Mark(in.Symbol())
		if err != nil {
			writeErr(w, 1001, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"result": map[string]any{
				"instrument_name": in.DeribitName(),
				"mark_price":      mk.Price,
				"mark_iv":         mk.MarkIV * 100,
				"bid_iv":          mk.BidIV * 100,
				"ask_iv":          mk.AskIV * 100,
				"greeks": map[string]any{
					"delta": mk.Delta,
					"gamma": mk.Gamma,
					"vega":  mk.Vega,
					"theta": mk.Theta,
					"rho":   mk.Rho,
				},
			},
		})
	})
	return mux
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ListenAndServe runs the public ticker until ctx is cancelled.
func ListenAndServe(ctx context.Context, addr string, book *optmarket.Market) error {
	return optlisten.Serve(ctx, addr, New(book))
}
