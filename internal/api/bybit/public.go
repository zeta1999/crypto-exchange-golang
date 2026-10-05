// Package bybit publishes the shared options book as Bybit public market data.
// Market data only: GET /v5/market/tickers?category=option. No order entry.
package bybit

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/api/optlisten"
	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// New serves the public option ticker for the shared book.
func New(book *optmarket.Market) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v5/market/tickers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("category") != "option" {
			write(w, 10001, "category must be option", nil)
			return
		}
		sym := r.URL.Query().Get("symbol")
		coin, strike, kind, expiry, err := optmarket.ParseBybitName(sym)
		if err != nil {
			write(w, 10001, err.Error(), nil)
			return
		}
		in, ok := book.Find(coin, strike, kind, expiry)
		if !ok {
			write(w, 10001, "symbol not found", nil)
			return
		}
		mk, err := book.Mark(in.Symbol())
		if err != nil {
			write(w, 10001, err.Error(), nil)
			return
		}
		write(w, 0, "OK", map[string]any{
			"category": "option",
			"list": []map[string]string{{
				"symbol":    in.BybitName(),
				"markPrice": num(mk.Price),
				"markIv":    num(mk.MarkIV),
				"bid1Iv":    num(mk.BidIV),
				"ask1Iv":    num(mk.AskIV),
				"delta":     num(mk.Delta),
				"gamma":     num(mk.Gamma),
				"vega":      num(mk.Vega),
				"theta":     num(mk.Theta),
			}},
		})
	})
	return mux
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', 8, 64) }

func write(w http.ResponseWriter, code int, msg string, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"retCode": code,
		"retMsg":  msg,
		"result":  result,
	})
}

// ListenAndServe runs the public ticker until ctx is cancelled.
func ListenAndServe(ctx context.Context, addr string, book *optmarket.Market) error {
	return optlisten.Serve(ctx, addr, New(book))
}
