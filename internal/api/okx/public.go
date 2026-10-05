// Package okx publishes the shared options book as OKX public market data.
// Market data only: GET /api/v5/public/opt-summary. No order entry.
package okx

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/api/optlisten"
	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// New serves the public option summary for the shared book.
func New(book *optmarket.Market) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/public/opt-summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		inst := r.URL.Query().Get("instId")
		coin, strike, kind, expiry, err := optmarket.ParseOKXName(inst)
		if err != nil {
			write(w, "51000", err.Error(), nil)
			return
		}
		in, ok := book.Find(coin, strike, kind, expiry)
		if !ok {
			write(w, "51001", "instrument not found", nil)
			return
		}
		mk, err := book.Mark(in.Symbol())
		if err != nil {
			write(w, "51001", err.Error(), nil)
			return
		}
		write(w, "0", "", []map[string]string{{
			"instId":  in.OKXName(),
			"markPx":  num(mk.Price),
			"markVol": num(mk.MarkIV),
			"bidVol":  num(mk.BidIV),
			"askVol":  num(mk.AskIV),
			"delta":   num(mk.Delta),
			"gamma":   num(mk.Gamma),
			"vega":    num(mk.Vega),
			"theta":   num(mk.Theta),
		}})
	})
	return mux
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', 8, 64) }

func write(w http.ResponseWriter, code, msg string, data any) {
	if data == nil {
		data = []any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg, "data": data})
}

// ListenAndServe runs the public summary until ctx is cancelled.
func ListenAndServe(ctx context.Context, addr string, book *optmarket.Market) error {
	return optlisten.Serve(ctx, addr, New(book))
}
