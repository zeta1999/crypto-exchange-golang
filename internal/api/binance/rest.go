package binance

import (
	"net/http"
	"strconv"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// qtyPrec / pricePrec are the fractional digits used when rendering quantities
// and prices on the wire. Binance quotes per-symbol filters; this subset uses a
// uniform high precision and trims trailing precision via decimal formatting.
const (
	qtyPrec   = 8
	pricePrec = 8
)

// --- public endpoints ---

// handlePing: GET /api/v3/ping -> {}.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	writeJSON(w, struct{}{})
}

// handleTime: GET /api/v3/time -> {"serverTime": <ms>}.
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	writeJSON(w, map[string]int64{"serverTime": s.now().UnixMilli()})
}

// exchangeInfoResponse mirrors GET /api/v3/exchangeInfo. It is the market
// discovery document a stock client (CCXT, python-binance) reads in
// loadMarkets() before placing any order. We expose the configured symbols
// with uniform precision filters derived from qtyPrec/pricePrec.
type exchangeInfoResponse struct {
	Timezone        string        `json:"timezone"`
	ServerTime      int64         `json:"serverTime"`
	RateLimits      []interface{} `json:"rateLimits"`
	ExchangeFilters []interface{} `json:"exchangeFilters"`
	Symbols         []symbolInfo  `json:"symbols"`
}

type symbolInfo struct {
	Symbol                 string        `json:"symbol"`
	Status                 string        `json:"status"`
	BaseAsset              string        `json:"baseAsset"`
	BaseAssetPrecision     int           `json:"baseAssetPrecision"`
	QuoteAsset             string        `json:"quoteAsset"`
	QuotePrecision         int           `json:"quotePrecision"`
	QuoteAssetPrecision    int           `json:"quoteAssetPrecision"`
	OrderTypes             []string      `json:"orderTypes"`
	IsSpotTradingAllowed   bool          `json:"isSpotTradingAllowed"`
	IsMarginTradingAllowed bool          `json:"isMarginTradingAllowed"`
	Filters                []interface{} `json:"filters"`
	Permissions            []string      `json:"permissions"`
}

// stepStr renders the smallest representable increment for a given fractional
// precision, e.g. prec 8 -> "0.00000001", prec 0 -> "1".
func stepStr(prec int) string {
	if prec <= 0 {
		return "1"
	}
	b := make([]byte, prec+2)
	b[0] = '0'
	b[1] = '.'
	for i := 2; i < prec+1; i++ {
		b[i] = '0'
	}
	b[prec+1] = '1'
	return string(b)
}

// handleExchangeInfo: GET /api/v3/exchangeInfo[?symbol=] (public). Returns all
// configured markets, or the single requested one (-1121 if unknown).
func (s *Server) handleExchangeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	want := r.URL.Query().Get("symbol")
	if want != "" {
		if _, ok := s.symbols.ToEngine(want); !ok {
			writeError(w, errInvalidSymbol())
			return
		}
	}
	priceStep := stepStr(pricePrec)
	qtyStep := stepStr(qtyPrec)
	syms := make([]symbolInfo, 0)
	for _, p := range s.symbols.Pairs() {
		if want != "" && p.Binance != want {
			continue
		}
		// base/quote come from the engine instrument ("BTC-USD" → BTC/USD), so a
		// client's unified symbol is "BTC/USD" even though the wire id is the
		// concatenated "BTCUSDT". This is intentional: the engine is USD-quoted.
		base, quote := splitEngineSymbol(p.Engine)
		syms = append(syms, symbolInfo{
			Symbol:               p.Binance,
			Status:               "TRADING",
			BaseAsset:            base,
			BaseAssetPrecision:   qtyPrec,
			QuoteAsset:           quote,
			QuotePrecision:       pricePrec,
			QuoteAssetPrecision:  pricePrec,
			OrderTypes:           []string{"LIMIT", "MARKET"},
			IsSpotTradingAllowed: true,
			Filters: []interface{}{
				map[string]string{"filterType": "PRICE_FILTER", "minPrice": priceStep, "maxPrice": "1000000000", "tickSize": priceStep},
				map[string]string{"filterType": "LOT_SIZE", "minQty": qtyStep, "maxQty": "1000000000", "stepSize": qtyStep},
				map[string]string{"filterType": "NOTIONAL", "minNotional": "0", "applyMinToMarket": "false"},
			},
			Permissions: []string{"SPOT"},
		})
	}
	writeJSON(w, exchangeInfoResponse{
		Timezone:        "UTC",
		ServerTime:      s.now().UnixMilli(),
		RateLimits:      []interface{}{},
		ExchangeFilters: []interface{}{},
		Symbols:         syms,
	})
}

// splitEngineSymbol splits "BTC-USD" into ("BTC","USD"). If there is no hyphen
// the whole string is treated as the base with an empty quote.
func splitEngineSymbol(eng string) (base, quote string) {
	for i := 0; i < len(eng); i++ {
		if eng[i] == '-' {
			return eng[:i], eng[i+1:]
		}
	}
	return eng, ""
}

// depthResponse mirrors GET /api/v3/depth. bids/asks are [price, qty] string
// pairs.
type depthResponse struct {
	LastUpdateID int64       `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

// handleDepth: GET /api/v3/depth?symbol=&limit= (public).
func (s *Server) handleDepth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	q := r.URL.Query()
	binSym := q.Get("symbol")
	if binSym == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	engSym, ok := s.symbols.ToEngine(binSym)
	if !ok {
		writeError(w, errInvalidSymbol())
		return
	}
	limit := 100
	if l := q.Get("limit"); l != "" {
		v, err := strconv.Atoi(l)
		if err != nil || v <= 0 {
			writeError(w, errIllegalParam("limit"))
			return
		}
		limit = v
	}
	snap, err := s.engine.Snapshot(engSym)
	if err != nil {
		writeError(w, errInvalidSymbol())
		return
	}
	resp := depthResponse{
		LastUpdateID: s.now().UnixMilli(),
		Bids:         levelsToPairs(snap.Bids, limit),
		Asks:         levelsToPairs(snap.Asks, limit),
	}
	writeJSON(w, resp)
}

func levelsToPairs(levels []orderbook.Level, limit int) [][2]string {
	if limit > len(levels) {
		limit = len(levels)
	}
	out := make([][2]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, [2]string{
			levels[i].Price.StringPrec(pricePrec),
			levels[i].Volume.StringPrec(qtyPrec),
		})
	}
	return out
}

// tickerPriceResponse mirrors GET /api/v3/ticker/price.
type tickerPriceResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

// handleTickerPrice: GET /api/v3/ticker/price?symbol= (public). Uses the last
// trade price when present, else the mid of best bid/ask.
func (s *Server) handleTickerPrice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	binSym := r.URL.Query().Get("symbol")
	if binSym == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	engSym, ok := s.symbols.ToEngine(binSym)
	if !ok {
		writeError(w, errInvalidSymbol())
		return
	}
	snap, err := s.engine.Snapshot(engSym)
	if err != nil {
		writeError(w, errInvalidSymbol())
		return
	}
	price := tickerPrice(snap)
	writeJSON(w, tickerPriceResponse{Symbol: binSym, Price: price.StringPrec(pricePrec)})
}

func tickerPrice(snap *orderbook.Snapshot) decimal.Decimal {
	if snap.LastTrade != nil {
		return snap.LastTrade.Price
	}
	hasBid := snap.BestBid.Sign() > 0
	hasAsk := snap.BestAsk.Sign() > 0
	switch {
	case hasBid && hasAsk:
		return snap.BestBid.Add(snap.BestAsk).Div(decimal.FromInt(2))
	case hasBid:
		return snap.BestBid
	case hasAsk:
		return snap.BestAsk
	default:
		return decimal.Zero
	}
}

// --- signed endpoints ---

// handleOrder multiplexes POST (place) and DELETE (cancel) on /api/v3/order.
func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePlaceOrder(w, r)
	case http.MethodDelete:
		s.handleCancelOrder(w, r)
	default:
		writeError(w, errIllegalParam("method"))
	}
}

// orderResponse is the Binance RESULT/ACK order shape (camelCase, prices/qtys
// as strings).
type orderResponse struct {
	Symbol              string `json:"symbol"`
	OrderID             int64  `json:"orderId"`
	OrderListID         int64  `json:"orderListId"`
	ClientOrderID       string `json:"clientOrderId"`
	TransactTime        int64  `json:"transactTime"`
	Price               string `json:"price"`
	OrigQty             string `json:"origQty"`
	ExecutedQty         string `json:"executedQty"`
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	Status              string `json:"status"`
	TimeInForce         string `json:"timeInForce"`
	Type                string `json:"type"`
	Side                string `json:"side"`
	Fills               []fill `json:"fills"`
}

type fill struct {
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
}

// handlePlaceOrder: POST /api/v3/order (SIGNED).
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	// Binance sends parameters as query string for SIGNED requests; also accept
	// form-encoded bodies for client compatibility.
	_ = r.ParseForm()
	get := func(k string) string {
		if v := r.URL.Query().Get(k); v != "" {
			return v
		}
		return r.PostFormValue(k)
	}

	binSym := get("symbol")
	if binSym == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	engSym, ok := s.symbols.ToEngine(binSym)
	if !ok {
		writeError(w, errInvalidSymbol())
		return
	}

	sideStr := get("side")
	side, err := parseSide(sideStr)
	if err != nil {
		writeError(w, err)
		return
	}

	typ := get("type")
	if typ == "" {
		writeError(w, errMandatoryParam("type"))
		return
	}

	qtyStr := get("quantity")
	if qtyStr == "" {
		writeError(w, errMandatoryParam("quantity"))
		return
	}
	qty, err := parseDecimalParam(qtyStr, "quantity")
	if err != nil {
		writeError(w, err)
		return
	}
	if qty.Sign() <= 0 {
		writeError(w, errIllegalParam("quantity"))
		return
	}

	tif := get("timeInForce")
	var price decimal.Decimal
	switch typ {
	case "LIMIT":
		if tif == "" {
			tif = "GTC"
		}
		priceStr := get("price")
		if priceStr == "" {
			writeError(w, errMandatoryParam("price"))
			return
		}
		price, err = parseDecimalParam(priceStr, "price")
		if err != nil {
			writeError(w, err)
			return
		}
		if price.Sign() <= 0 {
			writeError(w, errIllegalParam("price"))
			return
		}
	case "MARKET":
		tif = ""
	default:
		writeError(w, errIllegalParam("type"))
		return
	}

	// Reserve funds for a resting LIMIT order (a MARKET order never rests, so it
	// is settled from free balance on fill). buy locks quote (price*qty); sell
	// locks base (qty). Insufficient free balance rejects the order (-2010).
	var lockAsset string
	var lockAmt decimal.Decimal
	if s.ledger != nil && typ == "LIMIT" {
		base, quote := splitEngineSymbol(engSym)
		if side == orderbook.SideBuy {
			lockAsset, lockAmt = quote, price.Mul(qty)
		} else {
			lockAsset, lockAmt = base, qty
		}
		if !s.ledger.Lock(lockAsset, lockAmt) {
			writeError(w, errInsufficientBalance())
			return
		}
	}

	rec := s.registry.Record(binSym, engSym, sideStr, typ, tif, price, qty, get("newClientOrderId"))

	// Emit the NEW executionReport before placement so user-data subscribers see
	// the order acknowledged ahead of any synchronous fill (TRADE) reports the
	// book hook fires inside PlaceLimit/PlaceMarket.
	s.emitExecutionReport(rec.EngineID, execTypeNew, decimal.Zero, decimal.Zero)

	ord := &orderbook.Order{
		ID:         rec.EngineID,
		Instrument: engSym,
		Price:      price,
		Volume:     qty,
		Side:       side,
		IsMarket:   typ == "MARKET",
		Metadata:   map[string]string{"api": "binance", "clientOrderId": rec.ClientOrderID},
	}

	var trades []*orderbook.Trade
	if typ == "MARKET" {
		trades, _, err = s.engine.PlaceMarket(r.Context(), ord)
	} else {
		trades, _, err = s.engine.PlaceLimit(r.Context(), ord)
	}
	if err != nil {
		// The order is recorded BEFORE placement so a synchronous fill hook
		// (which fires inside PlaceLimit/PlaceMarket) isn't lost. If placement
		// itself failed, no trade fired, so roll the record back — otherwise it
		// lingers forever as a phantom NEW order in openOrders.
		s.registry.Remove(rec.OrderID)
		if s.ledger != nil {
			s.ledger.Unlock(lockAsset, lockAmt) // release the reservation (no-op for MARKET)
		}
		writeError(w, errInternal(err.Error()))
		return
	}

	// Build the fills array for the response. The registry's executedQty /
	// status are updated by the order book's synchronous "trade" hook
	// (registry.OnTrade), which already ran inside PlaceLimit/PlaceMarket for
	// both sides of each trade — so we deliberately do NOT ApplyFill here, to
	// avoid double-counting our own (taker) side.
	fills := make([]fill, 0, len(trades))
	for _, t := range trades {
		fills = append(fills, fill{
			Price:           t.Price.StringPrec(pricePrec),
			Qty:             t.Volume.StringPrec(qtyPrec),
			Commission:      "0",
			CommissionAsset: "",
		})
	}

	s.sleepAck(r.Context()) // artificial order-ack latency (Phase 7)
	snap, _ := s.registry.snapshot(rec.OrderID)
	writeJSON(w, orderResponse{
		Symbol:              binSym,
		OrderID:             snap.OrderID,
		OrderListID:         -1,
		ClientOrderID:       snap.ClientOrderID,
		TransactTime:        s.now().UnixMilli(),
		Price:               snap.Price.StringPrec(pricePrec),
		OrigQty:             snap.OrigQty.StringPrec(qtyPrec),
		ExecutedQty:         snap.ExecutedQty.StringPrec(qtyPrec),
		CummulativeQuoteQty: snap.CummulativeQuoteQty.StringPrec(pricePrec),
		Status:              snap.Status,
		TimeInForce:         snap.TimeInForce,
		Type:                snap.Type,
		Side:                snap.Side,
		Fills:               fills,
	})
}

// handleCancelOrder: DELETE /api/v3/order (SIGNED) by orderId or
// origClientOrderId.
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	binSym := q.Get("symbol")
	if binSym == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	if _, ok := s.symbols.ToEngine(binSym); !ok {
		writeError(w, errInvalidSymbol())
		return
	}

	var rec *orderRecord
	if idStr := q.Get("orderId"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, errIllegalParam("orderId"))
			return
		}
		got, ok := s.registry.Get(id)
		if !ok {
			writeError(w, errUnknownOrder())
			return
		}
		rec = got
	} else if coid := q.Get("origClientOrderId"); coid != "" {
		got, ok := s.registry.GetByClientOrderID(coid)
		if !ok {
			writeError(w, errUnknownOrder())
			return
		}
		rec = got
	} else {
		writeError(w, errMandatoryParam("orderId"))
		return
	}

	_, err := s.engine.CancelOrder(r.Context(), rec.EngineSymbol, rec.EngineID)
	if err != nil {
		// Already filled or never rested: a fully-filled order is gone from the
		// book. If it is already terminal, surface its state; otherwise reject.
		cur, _ := s.registry.snapshot(rec.OrderID)
		if cur.Status == statusFilled || cur.Status == statusCanceled {
			writeJSON(w, canceledResponse(binSym, cur))
			return
		}
		writeError(w, errUnknownOrder())
		return
	}
	s.registry.MarkCanceled(rec.OrderID)
	cur, _ := s.registry.snapshot(rec.OrderID)
	writeJSON(w, canceledResponse(binSym, cur))
}

// canceledOrderResponse is the DELETE /api/v3/order shape.
type canceledOrderResponse struct {
	Symbol              string `json:"symbol"`
	OrigClientOrderID   string `json:"origClientOrderId"`
	OrderID             int64  `json:"orderId"`
	OrderListID         int64  `json:"orderListId"`
	ClientOrderID       string `json:"clientOrderId"`
	Price               string `json:"price"`
	OrigQty             string `json:"origQty"`
	ExecutedQty         string `json:"executedQty"`
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	Status              string `json:"status"`
	TimeInForce         string `json:"timeInForce"`
	Type                string `json:"type"`
	Side                string `json:"side"`
}

func canceledResponse(binSym string, rec orderRecord) canceledOrderResponse {
	return canceledOrderResponse{
		Symbol:              binSym,
		OrigClientOrderID:   rec.ClientOrderID,
		OrderID:             rec.OrderID,
		OrderListID:         -1,
		ClientOrderID:       rec.ClientOrderID,
		Price:               rec.Price.StringPrec(pricePrec),
		OrigQty:             rec.OrigQty.StringPrec(qtyPrec),
		ExecutedQty:         rec.ExecutedQty.StringPrec(qtyPrec),
		CummulativeQuoteQty: rec.CummulativeQuoteQty.StringPrec(pricePrec),
		Status:              statusCanceled,
		TimeInForce:         rec.TimeInForce,
		Type:                rec.Type,
		Side:                rec.Side,
	}
}

// openOrderResponse is one element of GET /api/v3/openOrders.
type openOrderResponse struct {
	Symbol              string `json:"symbol"`
	OrderID             int64  `json:"orderId"`
	OrderListID         int64  `json:"orderListId"`
	ClientOrderID       string `json:"clientOrderId"`
	Price               string `json:"price"`
	OrigQty             string `json:"origQty"`
	ExecutedQty         string `json:"executedQty"`
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	Status              string `json:"status"`
	TimeInForce         string `json:"timeInForce"`
	Type                string `json:"type"`
	Side                string `json:"side"`
	Time                int64  `json:"time"`
	UpdateTime          int64  `json:"updateTime"`
	IsWorking           bool   `json:"isWorking"`
}

// handleOpenOrders: GET /api/v3/openOrders?symbol= (SIGNED).
func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	engSym := ""
	if binSym := r.URL.Query().Get("symbol"); binSym != "" {
		e, ok := s.symbols.ToEngine(binSym)
		if !ok {
			writeError(w, errInvalidSymbol())
			return
		}
		engSym = e
	}
	records := s.registry.OpenOrders(engSym)
	out := make([]openOrderResponse, 0, len(records))
	for _, rec := range records {
		out = append(out, openOrderResponse{
			Symbol:              rec.BinanceSymbol,
			OrderID:             rec.OrderID,
			OrderListID:         -1,
			ClientOrderID:       rec.ClientOrderID,
			Price:               rec.Price.StringPrec(pricePrec),
			OrigQty:             rec.OrigQty.StringPrec(qtyPrec),
			ExecutedQty:         rec.ExecutedQty.StringPrec(qtyPrec),
			CummulativeQuoteQty: rec.CummulativeQuoteQty.StringPrec(pricePrec),
			Status:              rec.Status,
			TimeInForce:         rec.TimeInForce,
			Type:                rec.Type,
			Side:                rec.Side,
			Time:                rec.Time,
			UpdateTime:          rec.UpdateTime,
			IsWorking:           true,
		})
	}
	writeJSON(w, out)
}

// accountResponse is a minimal GET /api/v3/account shape. Commissions and
// balances are stubs: the engine has no ledger, so balances come from static
// config (default empty) and commissions are zero.
type accountResponse struct {
	MakerCommission  int       `json:"makerCommission"`
	TakerCommission  int       `json:"takerCommission"`
	BuyerCommission  int       `json:"buyerCommission"`
	SellerCommission int       `json:"sellerCommission"`
	CanTrade         bool      `json:"canTrade"`
	CanWithdraw      bool      `json:"canWithdraw"`
	CanDeposit       bool      `json:"canDeposit"`
	UpdateTime       int64     `json:"updateTime"`
	AccountType      string    `json:"accountType"`
	Balances         []Balance `json:"balances"`
	Permissions      []string  `json:"permissions"`
}

// ledgerBalances renders the ledger snapshot as Binance balance entries.
func ledgerBalances(l *account.Ledger) []Balance {
	snap := l.Snapshot()
	out := make([]Balance, 0, len(snap))
	for _, b := range snap {
		out = append(out, Balance{
			Asset:  b.Asset,
			Free:   b.Free.StringPrec(qtyPrec),
			Locked: b.Locked.StringPrec(qtyPrec),
		})
	}
	return out
}

// handleAccount: GET /api/v3/account (SIGNED).
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errIllegalParam("method"))
		return
	}
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	// Prefer live ledger balances; fall back to the static WithBalances stub.
	balances := s.balances
	if s.ledger != nil {
		balances = ledgerBalances(s.ledger)
	}
	if balances == nil {
		balances = []Balance{}
	}
	writeJSON(w, accountResponse{
		CanTrade:    true,
		CanWithdraw: true,
		CanDeposit:  true,
		UpdateTime:  s.now().UnixMilli(),
		AccountType: "SPOT",
		Balances:    balances,
		Permissions: []string{"SPOT"},
	})
}

// --- param helpers ---

func parseSide(s string) (orderbook.Side, error) {
	switch s {
	case "BUY":
		return orderbook.SideBuy, nil
	case "SELL":
		return orderbook.SideSell, nil
	case "":
		return "", errMandatoryParam("side")
	default:
		return "", errIllegalParam("side")
	}
}

// parseDecimalParam parses a Binance string/number quantity or price. It rejects
// malformed and non-finite input.
func parseDecimalParam(s, name string) (decimal.Decimal, error) {
	// Parse the exact decimal string only. We deliberately do NOT fall back to
	// strconv.ParseFloat: it accepts "NaN"/"Inf" (which panic decimal.FromFloat)
	// and huge exponents (which overflow the 128-bit range and panic), turning
	// trivially-crafted input into a handler panic. Binance clients send plain
	// decimal strings, which decimal.Parse handles.
	d, err := decimal.Parse(s)
	if err != nil {
		return decimal.Zero, errIllegalParam(name)
	}
	return d, nil
}
