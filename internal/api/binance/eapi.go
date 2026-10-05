package binance

import (
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
)

// MarkData is one element of GET /eapi/v1/mark. No rho: Binance EAPI omits it.
type MarkData struct {
	Symbol           string `json:"symbol"`
	MarkPrice        string `json:"markPrice"`
	BidIV            string `json:"bidIV"`
	AskIV            string `json:"askIV"`
	MarkIV           string `json:"markIV"`
	Delta            string `json:"delta"`
	Theta            string `json:"theta"`
	Gamma            string `json:"gamma"`
	Vega             string `json:"vega"`
	HighPriceLimit   string `json:"highPriceLimit"`
	LowPriceLimit    string `json:"lowPriceLimit"`
	RiskFreeInterest string `json:"riskFreeInterest"`
}

// Depth is GET /eapi/v1/depth.
type Depth struct {
	T    int64       `json:"T"`
	U    int64       `json:"u"`
	Bids [][2]string `json:"bids"`
	Asks [][2]string `json:"asks"`
}

// OptionSymbolInfo is one optionSymbols entry of GET /eapi/v1/exchangeInfo.
type OptionSymbolInfo struct {
	Symbol        string `json:"symbol"`
	Underlying    string `json:"underlying"`
	QuoteAsset    string `json:"quoteAsset"`
	StrikePrice   string `json:"strikePrice"`
	ExpiryDate    int64  `json:"expiryDate"`
	Side          string `json:"side"`
	Unit          int    `json:"unit"`
	PriceScale    int    `json:"priceScale"`
	QuantityScale int    `json:"quantityScale"`
}

// ExchangeInfo is GET /eapi/v1/exchangeInfo (options subset).
type ExchangeInfo struct {
	Timezone      string             `json:"timezone"`
	ServerTime    int64              `json:"serverTime"`
	OptionSymbols []OptionSymbolInfo `json:"optionSymbols"`
}

// IndexData is GET /eapi/v1/index.
type IndexData struct {
	IndexPrice string `json:"indexPrice"`
	Time       int64  `json:"time"`
}

func eapiNum(f float64) string { return strconv.FormatFloat(f, 'f', 8, 64) }

// ProjectMark maps a book mark onto an EAPI mark. Rho is dropped.
func ProjectMark(mk optmarket.Mark) MarkData {
	return MarkData{
		Symbol:           mk.Symbol,
		MarkPrice:        eapiNum(mk.Price),
		BidIV:            eapiNum(mk.BidIV),
		AskIV:            eapiNum(mk.AskIV),
		MarkIV:           eapiNum(mk.MarkIV),
		Delta:            eapiNum(mk.Delta),
		Theta:            eapiNum(mk.Theta),
		Gamma:            eapiNum(mk.Gamma),
		Vega:             eapiNum(mk.Vega),
		HighPriceLimit:   eapiNum(mk.HighPriceLimit),
		LowPriceLimit:    eapiNum(mk.LowPriceLimit),
		RiskFreeInterest: eapiNum(mk.Rate),
	}
}

// ProjectMarks maps every book mark, in order.
func ProjectMarks(mks []optmarket.Mark) []MarkData {
	out := make([]MarkData, len(mks))
	for i, mk := range mks {
		out[i] = ProjectMark(mk)
	}
	return out
}

// ProjectDepth maps a synthetic book onto EAPI depth.
func ProjectDepth(b optmarket.Book) Depth {
	d := Depth{T: b.TimeMs, U: b.UpdateID, Bids: [][2]string{}, Asks: [][2]string{}}
	for _, lv := range b.Bids {
		d.Bids = append(d.Bids, [2]string{eapiNum(lv.Price), eapiNum(lv.Size)})
	}
	for _, lv := range b.Asks {
		d.Asks = append(d.Asks, [2]string{eapiNum(lv.Price), eapiNum(lv.Size)})
	}
	return d
}

// ProjectExchangeInfo maps the contract list onto EAPI exchangeInfo.
// serverTime is the book clock already stored on the spot, or the caller passes now.
func ProjectExchangeInfo(contracts []optmarket.Contract, serverTime int64) ExchangeInfo {
	syms := make([]OptionSymbolInfo, len(contracts))
	for i, c := range contracts {
		syms[i] = OptionSymbolInfo{
			Symbol:        c.Symbol,
			Underlying:    c.Underlying,
			QuoteAsset:    c.Quote,
			StrikePrice:   strconv.FormatFloat(c.Strike, 'f', -1, 64),
			ExpiryDate:    c.ExpiryMs,
			Side:          c.Side,
			Unit:          c.Unit,
			PriceScale:    c.PriceScale,
			QuantityScale: c.QuantityScale,
		}
	}
	return ExchangeInfo{Timezone: "UTC", ServerTime: serverTime, OptionSymbols: syms}
}

// ProjectIndex maps a spot print onto EAPI index.
func ProjectIndex(s optmarket.Spot) IndexData {
	return IndexData{IndexPrice: eapiNum(s.Price), Time: s.TimeMs}
}
