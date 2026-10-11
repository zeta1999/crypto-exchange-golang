package binance

import (
	"net/http"
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// WithOptionsMarket enables the Binance-EAPI-compatible options surface (CR-9
// + order entry): GET /eapi/v1/{exchangeInfo,mark,mark/exotic,depth,index,
// orders} (public market data) and POST/GET/DELETE /eapi/v1/order (SIGNED
// option order entry, LIMIT-only, against the engine book on which the option
// symbols are registered). When nil, no /eapi routes are registered.
func WithOptionsMarket(om *optmarket.Market) Option {
	return func(s *Server) { s.optMarket = om }
}

// handleOptionExchangeInfo: GET /eapi/v1/exchangeInfo — option contract specs.
func (s *Server) handleOptionExchangeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	contracts := s.optMarket.Contracts()
	now := int64(0)
	if len(contracts) > 0 {
		if spot, err := s.optMarket.Index(contracts[0].Underlying); err == nil {
			now = spot.TimeMs
		}
	}
	writeJSON(w, ProjectExchangeInfo(contracts, now))
}

// handleOptionMark: GET /eapi/v1/mark[?symbol=] — vanilla BLACK–SCHOLES mark
// price + IV + greeks. Flow EXOTICS are excluded here (they are not BS
// instruments); they are served by /eapi/v1/mark/exotic. Always returns a JSON
// ARRAY (one element when ?symbol is given), matching EAPI.
func (s *Server) handleOptionMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	if sym := r.URL.Query().Get("symbol"); sym != "" {
		mk, err := s.optMarket.Mark(sym)
		if err != nil {
			writeError(w, errInvalidSymbol())
			return
		}
		if mk.Pricer != "" {
			writeError(w, errIllegalParam("symbol")) // exotic: use /eapi/v1/mark/exotic
			return
		}
		writeJSON(w, []MarkData{ProjectMark(mk)})
		return
	}
	mks := s.optMarket.Marks()
	out := make([]MarkData, 0, len(mks))
	for _, m := range mks {
		if m.Pricer == "" { // vanilla only
			out = append(out, ProjectMark(m))
		}
	}
	writeJSON(w, out)
}

// handleOptionExoticMark: GET /eapi/v1/mark/exotic[?symbol=] — flow-exotic
// marks, with pricing provenance (pricer backend + engine model) and the
// executable bid/ask that bracket the single Monte-Carlo fair value. IV/greeks
// are always "0" (no implied vol is invented). Always returns a JSON array
// (one element when ?symbol is given). Exotics share the same underlying index,
// book half-spread, and price band as the vanilla chain.
func (s *Server) handleOptionExoticMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	if sym := r.URL.Query().Get("symbol"); sym != "" {
		mk, err := s.optMarket.Mark(sym)
		if err != nil {
			writeError(w, errInvalidSymbol())
			return
		}
		if mk.Pricer == "" {
			writeError(w, errIllegalParam("symbol")) // vanilla: use /eapi/v1/mark
			return
		}
		writeJSON(w, []ExoticMarkData{projectExoticMark(mk)})
		return
	}
	out := make([]ExoticMarkData, 0)
	for _, mk := range s.optMarket.Marks() {
		if mk.Pricer != "" {
			out = append(out, projectExoticMark(mk))
		}
	}
	writeJSON(w, out)
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
	writeJSON(w, ProjectDepth(d))
}

// ExoticMarkData is one element of GET /eapi/v1/mark/exotic. Mark/Price and the
// band carry the single Monte-Carlo fair value and its executable bracket;
// BidIV/AskIV/MarkIV stay "0" (no implied vol is invented for an exotic) and
// Pricer/Model state provenance.
type ExoticMarkData struct {
	Symbol           string `json:"symbol"`
	Price            string `json:"price"`
	Bid              string `json:"bid"`
	Ask              string `json:"ask"`
	MarkIV           string `json:"markIV"` // always "0" — not a BS instrument
	BidIV            string `json:"bidIV"`  // always "0"
	AskIV            string `json:"askIV"`  // always "0"
	HighPriceLimit   string `json:"highPriceLimit"`
	LowPriceLimit    string `json:"lowPriceLimit"`
	Pricer           string `json:"pricer"` // backend name, e.g. "saga"
	Model            string `json:"model"`  // engine model, e.g. "bs"
	RiskFreeInterest string `json:"riskFreeInterest"`
}

// projectExoticMark maps an optmarket exotic mark onto its EAPI shape.
func projectExoticMark(mk optmarket.Mark) ExoticMarkData {
	return ExoticMarkData{
		Symbol: mk.Symbol,
		Price:  eapiNum(mk.Price),
		Bid:    eapiNum(mk.Bid),
		Ask:    eapiNum(mk.Ask),
		MarkIV: eapiNum(0), BidIV: eapiNum(0), AskIV: eapiNum(0),
		HighPriceLimit:   eapiNum(mk.HighPriceLimit),
		LowPriceLimit:    eapiNum(mk.LowPriceLimit),
		Pricer:           mk.Pricer,
		Model:            mk.Model,
		RiskFreeInterest: eapiNum(mk.Rate),
	}
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
	writeJSON(w, ProjectIndex(idx))
}
