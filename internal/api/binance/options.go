package binance

import (
	"net/http"
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// WithOptionsMarket enables the Binance-EAPI-compatible options market-data
// surface (CR-9): GET /eapi/v1/{exchangeInfo,mark,depth,index}. Market data only
// — these endpoints are public (unsigned), like /api/v3/depth. Options ORDER
// ENTRY is not wired here (the emulator's matching engine is spot; options are a
// priced data surface for now). When nil, no /eapi routes are registered.
func WithOptionsMarket(om *optmarket.Market) Option {
	return func(s *Server) { s.optMarket = om }
}

// handleOptionExchangeInfo: GET /eapi/v1/exchangeInfo — option contract specs.
func (s *Server) handleOptionExchangeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	writeJSON(w, s.optMarket.ExchangeInfo())
}

// handleOptionMark: GET /eapi/v1/mark[?symbol=] — mark price + IV + greeks.
// Always returns a JSON ARRAY (one element when ?symbol is given), matching EAPI.
func (s *Server) handleOptionMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	if sym := r.URL.Query().Get("symbol"); sym != "" {
		md, err := s.optMarket.Mark(sym)
		if err != nil {
			writeError(w, errInvalidSymbol())
			return
		}
		writeJSON(w, []optmarket.MarkData{md})
		return
	}
	writeJSON(w, s.optMarket.MarkAll())
}

// handleOptionDepth: GET /eapi/v1/depth?symbol=&limit= — synthetic order book.
func (s *Server) handleOptionDepth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	sym := r.URL.Query().Get("symbol")
	if sym == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, errIllegalParam("limit"))
			return
		}
		limit = n
	}
	d, err := s.optMarket.Depth(sym, limit)
	if err != nil {
		writeError(w, errInvalidSymbol())
		return
	}
	writeJSON(w, d)
}

// handleOptionIndex: GET /eapi/v1/index?underlying= — spot index for the pair.
func (s *Server) handleOptionIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	underlying := r.URL.Query().Get("underlying")
	if underlying == "" {
		writeError(w, errMandatoryParam("underlying"))
		return
	}
	idx, err := s.optMarket.Index(underlying)
	if err != nil {
		writeError(w, errInvalidSymbol())
		return
	}
	writeJSON(w, idx)
}
