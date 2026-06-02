package coinbase

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// sizePrec / pricePrec are the fractional digits used when rendering sizes and
// prices on the wire. Coinbase quotes per-product increments; this subset uses
// a uniform high precision and trims trailing precision via decimal formatting.
const (
	sizePrec  = 8
	pricePrec = 8
)

const maxBodyBytes = 1 << 20 // 1 MiB cap on signed request bodies

// --- public endpoints ---

// handleTime: GET /api/v3/brokerage/time (public).
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	now := s.now()
	writeJSON(w, map[string]string{
		"iso":          now.UTC().Format(time.RFC3339),
		"epochSeconds": strconv.FormatInt(now.Unix(), 10),
		"epochMillis":  strconv.FormatInt(now.UnixMilli(), 10),
	})
}

// priceBookEntry is one [price,size] level in a product book.
type priceBookEntry struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// priceBook is the body of a product_book response.
type priceBook struct {
	ProductID string           `json:"product_id"`
	Bids      []priceBookEntry `json:"bids"`
	Asks      []priceBookEntry `json:"asks"`
	Time      string           `json:"time"`
}

type productBookResponse struct {
	PriceBook priceBook `json:"pricebook"`
}

// handleProductBook: GET /api/v3/brokerage/product_book?product_id=&limit=
// (public).
func (s *Server) handleProductBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	q := r.URL.Query()
	productID := q.Get("product_id")
	if productID == "" {
		writeError(w, errInvalidArgument("product_id is required"))
		return
	}
	engSym, ok := s.products.Resolve(productID)
	if !ok {
		writeError(w, errInvalidProduct(productID))
		return
	}
	limit := 0 // 0 => all levels
	if l := q.Get("limit"); l != "" {
		v, err := strconv.Atoi(l)
		if err != nil || v < 0 {
			writeError(w, errInvalidArgument("invalid limit"))
			return
		}
		limit = v
	}
	snap, err := s.engine.Snapshot(engSym)
	if err != nil {
		writeError(w, errInvalidProduct(productID))
		return
	}
	writeJSON(w, productBookResponse{PriceBook: priceBook{
		ProductID: productID,
		Bids:      levelsToEntries(snap.Bids, limit),
		Asks:      levelsToEntries(snap.Asks, limit),
		Time:      s.now().UTC().Format(time.RFC3339),
	}})
}

// levelsToEntries renders order-book levels as Coinbase price/size entries. A
// limit of 0 means all levels.
func levelsToEntries(levels []orderbook.Level, limit int) []priceBookEntry {
	n := len(levels)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]priceBookEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, priceBookEntry{
			Price: levels[i].Price.StringPrec(pricePrec),
			Size:  levels[i].Volume.StringPrec(sizePrec),
		})
	}
	return out
}

// productResponse is the GET .../products[/{product_id}] shape. It carries the
// increment and min-size fields CCXT's parseMarket reads to populate amount /
// price precision and order limits; trading_disabled drives market['active'].
type productResponse struct {
	ProductID       string `json:"product_id"`
	Price           string `json:"price"`
	Status          string `json:"status"`
	TradingDisabled bool   `json:"trading_disabled"`
	QuoteCur        string `json:"quote_currency_id"`
	BaseCur         string `json:"base_currency_id"`
	BaseIncrement   string `json:"base_increment"`
	QuoteIncrement  string `json:"quote_increment"`
	BaseMinSize     string `json:"base_min_size"`
	QuoteMinSize    string `json:"quote_min_size"`
}

// stepStr renders the smallest representable increment for a fractional
// precision, e.g. 8 -> "0.00000001". Used to advertise base/quote increments.
func stepStr(prec int) string {
	if prec <= 0 {
		return "1"
	}
	b := make([]byte, prec+2)
	b[0], b[1] = '0', '.'
	for i := 2; i <= prec; i++ {
		b[i] = '0'
	}
	b[prec+1] = '1'
	return string(b)
}

// productsListResponse mirrors GET /api/v3/brokerage/products — the market
// discovery document a stock client (CCXT) reads in loadMarkets() before
// placing any order.
type productsListResponse struct {
	Products    []productResponse `json:"products"`
	NumProducts int               `json:"num_products"`
}

// handleProducts: GET /api/v3/brokerage/products (public). Lists every
// configured product so a client can discover the tradable markets.
func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	ids := s.products.List()
	out := make([]productResponse, 0, len(ids))
	for _, productID := range ids {
		engSym, ok := s.products.Resolve(productID)
		if !ok {
			continue
		}
		price := "0"
		if snap, err := s.engine.Snapshot(engSym); err == nil {
			price = tickerPrice(snap).StringPrec(pricePrec)
		}
		out = append(out, newProductResponse(productID, price))
	}
	writeJSON(w, productsListResponse{Products: out, NumProducts: len(out)})
}

// handleProduct: GET /api/v3/brokerage/products/{product_id} (public). Uses the
// last trade price when present, else the mid of best bid/ask.
func (s *Server) handleProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	productID := strings.TrimPrefix(r.URL.Path, "/api/v3/brokerage/products/")
	if productID == "" || strings.Contains(productID, "/") {
		writeError(w, errInvalidArgument("product_id is required"))
		return
	}
	engSym, ok := s.products.Resolve(productID)
	if !ok {
		writeError(w, errInvalidProduct(productID))
		return
	}
	snap, err := s.engine.Snapshot(engSym)
	if err != nil {
		writeError(w, errInvalidProduct(productID))
		return
	}
	writeJSON(w, newProductResponse(productID, tickerPrice(snap).StringPrec(pricePrec)))
}

// newProductResponse builds the public product document for productID at the
// given price string, advertising the uniform base/quote increments (and a
// permissive min size) so a stock client can derive precision and limits.
func newProductResponse(productID, price string) productResponse {
	base, quote := splitProduct(productID)
	return productResponse{
		ProductID:       productID,
		Price:           price,
		Status:          "online",
		TradingDisabled: false,
		QuoteCur:        quote,
		BaseCur:         base,
		BaseIncrement:   stepStr(sizePrec),
		QuoteIncrement:  stepStr(pricePrec),
		BaseMinSize:     stepStr(sizePrec),
		QuoteMinSize:    "0",
	}
}

// splitProduct splits "BTC-USD" into ("BTC","USD"); a product without a hyphen
// yields the whole string as base and an empty quote.
func splitProduct(productID string) (base, quote string) {
	if i := strings.IndexByte(productID, '-'); i >= 0 {
		return productID[:i], productID[i+1:]
	}
	return productID, ""
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

// readSignedBody reads (and caps) the request body, verifies the signature over
// it, and returns the raw bytes for the handler to decode. The body must be
// read before verification because the HMAC covers it.
func (s *Server) readSignedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, errInvalidArgument("could not read request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := s.auth.Verify(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// orderConfiguration is the Coinbase order_configuration union. Exactly one of
// the two sub-objects is populated.
type orderConfiguration struct {
	LimitLimitGTC *limitGTCConfig  `json:"limit_limit_gtc,omitempty"`
	MarketIOC     *marketIOCConfig `json:"market_market_ioc,omitempty"`
}

type limitGTCConfig struct {
	BaseSize   string `json:"base_size"`
	LimitPrice string `json:"limit_price"`
	PostOnly   bool   `json:"post_only"`
}

type marketIOCConfig struct {
	BaseSize string `json:"base_size"`
}

// createOrderRequest is the POST .../orders body.
type createOrderRequest struct {
	ClientOrderID      string             `json:"client_order_id"`
	ProductID          string             `json:"product_id"`
	Side               string             `json:"side"`
	OrderConfiguration orderConfiguration `json:"order_configuration"`
}

// successResponse is the success_response sub-object.
type successResponse struct {
	OrderID       string `json:"order_id"`
	ProductID     string `json:"product_id"`
	Side          string `json:"side"`
	ClientOrderID string `json:"client_order_id"`
}

// errorResponse is the error_response sub-object.
type errorResponse struct {
	Error                 string `json:"error"`
	Message               string `json:"message"`
	ErrorDetails          string `json:"error_details,omitempty"`
	PreviewFailureReason  string `json:"preview_failure_reason,omitempty"`
	NewOrderFailureReason string `json:"new_order_failure_reason,omitempty"`
}

// createOrderResponse is the POST .../orders response envelope.
type createOrderResponse struct {
	Success            bool                `json:"success"`
	SuccessResponse    *successResponse    `json:"success_response,omitempty"`
	ErrorResponse      *errorResponse      `json:"error_response,omitempty"`
	OrderConfiguration *orderConfiguration `json:"order_configuration,omitempty"`
}

// handleCreateOrder: POST /api/v3/brokerage/orders (SIGNED).
func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	body, err := s.readSignedBody(r)
	if err != nil {
		writeError(w, err)
		return
	}

	var req createOrderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, errInvalidArgument("malformed JSON body"))
		return
	}

	engSym, ok := s.products.Resolve(req.ProductID)
	if !ok {
		writeError(w, errInvalidProduct(req.ProductID))
		return
	}

	side, err := parseSide(req.Side)
	if err != nil {
		writeError(w, err)
		return
	}

	// Exactly one order_configuration variant must be set.
	cfg := req.OrderConfiguration
	if (cfg.LimitLimitGTC == nil) == (cfg.MarketIOC == nil) {
		writeError(w, errInvalidArgument("order_configuration must set exactly one of limit_limit_gtc or market_market_ioc"))
		return
	}

	var (
		orderType string
		isMarket  bool
		postOnly  bool
		price     decimal.Decimal
		size      decimal.Decimal
	)
	if cfg.LimitLimitGTC != nil {
		orderType = "LIMIT"
		postOnly = cfg.LimitLimitGTC.PostOnly
		size, err = parseDecimalField(cfg.LimitLimitGTC.BaseSize, "base_size")
		if err != nil {
			writeError(w, err)
			return
		}
		price, err = parseDecimalField(cfg.LimitLimitGTC.LimitPrice, "limit_price")
		if err != nil {
			writeError(w, err)
			return
		}
		if price.Sign() <= 0 {
			writeError(w, errInvalidArgument("limit_price must be positive"))
			return
		}
	} else {
		orderType = "MARKET"
		isMarket = true
		size, err = parseDecimalField(cfg.MarketIOC.BaseSize, "base_size")
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if size.Sign() <= 0 {
		writeError(w, errInvalidArgument("base_size must be positive"))
		return
	}

	rec := s.registry.Record(req.ProductID, req.Side, orderType, postOnly, price, size, req.ClientOrderID)

	ord := &orderbook.Order{
		ID:         rec.EngineID,
		Instrument: engSym,
		Price:      price,
		Volume:     size,
		Side:       side,
		IsMarket:   isMarket,
		Metadata:   map[string]string{"api": "coinbase", "clientOrderId": rec.ClientOrderID},
	}

	if isMarket {
		_, _, err = s.engine.PlaceMarket(r.Context(), ord)
	} else {
		_, _, err = s.engine.PlaceLimit(r.Context(), ord)
	}
	if err != nil {
		// Recorded BEFORE placement so a synchronous fill hook (which fires
		// inside PlaceLimit/PlaceMarket) isn't lost. Placement itself failed, so
		// no trade fired — roll the record back to avoid a phantom OPEN order.
		s.registry.Remove(rec.OrderID)
		writeJSON(w, createOrderResponse{
			Success: false,
			ErrorResponse: &errorResponse{
				Error:                 errInternal,
				Message:               err.Error(),
				NewOrderFailureReason: "UNKNOWN_FAILURE_REASON",
			},
		})
		return
	}

	// The registry's filled_size/status are updated by the order book's
	// synchronous "trade" hook (registry.OnTrade), which already ran inside
	// PlaceLimit/PlaceMarket for both sides of each trade.
	//
	// Surface the order's resulting state on the WS user channel. The book
	// "trade"/"cancel" hooks emit intermediate fills/cancels; this final emit
	// covers the initial OPEN state of a resting order and the FILLED state of
	// a market order (whose resting counterpart, not itself, drives the hook).
	s.EmitUserByOrderID(rec.OrderID)

	s.sleepAck(r.Context()) // artificial order-ack latency (Phase 7)
	echoCfg := cfg
	writeJSON(w, createOrderResponse{
		Success: true,
		SuccessResponse: &successResponse{
			OrderID:       rec.OrderID,
			ProductID:     req.ProductID,
			Side:          req.Side,
			ClientOrderID: rec.ClientOrderID,
		},
		OrderConfiguration: &echoCfg,
	})
}

// batchCancelRequest is the POST .../orders/batch_cancel body.
type batchCancelRequest struct {
	OrderIDs []string `json:"order_ids"`
}

// cancelResult is one element of the batch_cancel results array.
type cancelResult struct {
	Success       bool   `json:"success"`
	FailureReason string `json:"failure_reason"`
	OrderID       string `json:"order_id"`
}

type batchCancelResponse struct {
	Results []cancelResult `json:"results"`
}

// handleBatchCancel: POST /api/v3/brokerage/orders/batch_cancel (SIGNED).
func (s *Server) handleBatchCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	body, err := s.readSignedBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req batchCancelRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, errInvalidArgument("malformed JSON body"))
		return
	}

	results := make([]cancelResult, 0, len(req.OrderIDs))
	for _, oid := range req.OrderIDs {
		results = append(results, s.cancelOne(r, oid))
	}
	writeJSON(w, batchCancelResponse{Results: results})
}

// cancelOne cancels a single order by Coinbase order_id and reports the result.
func (s *Server) cancelOne(r *http.Request, oid string) cancelResult {
	rec, ok := s.registry.Get(oid)
	if !ok {
		return cancelResult{Success: false, OrderID: oid, FailureReason: errUnknownCancel}
	}
	cur, _ := s.registry.snapshot(oid)
	if cur.Status == statusCancelled {
		return cancelResult{Success: false, OrderID: oid, FailureReason: "DUPLICATE_CANCEL_REQUEST"}
	}
	if cur.Status == statusFilled {
		return cancelResult{Success: false, OrderID: oid, FailureReason: "INVALID_CANCEL_REQUEST"}
	}
	if _, err := s.engine.CancelOrder(r.Context(), rec.ProductID, rec.EngineID); err != nil {
		// Not resting: either already terminal (race) or never rested.
		again, _ := s.registry.snapshot(oid)
		if again.Status == statusFilled {
			return cancelResult{Success: false, OrderID: oid, FailureReason: "INVALID_CANCEL_REQUEST"}
		}
		return cancelResult{Success: false, OrderID: oid, FailureReason: errUnknownCancel}
	}
	s.registry.MarkCancelled(oid)
	return cancelResult{Success: true, OrderID: oid}
}

// orderView is the Coinbase order shape returned by the historical endpoints.
type orderView struct {
	OrderID              string             `json:"order_id"`
	ClientOrderID        string             `json:"client_order_id"`
	ProductID            string             `json:"product_id"`
	Side                 string             `json:"side"`
	Status               string             `json:"status"`
	OrderConfiguration   orderConfiguration `json:"order_configuration"`
	FilledSize           string             `json:"filled_size"`
	AverageFilledPrice   string             `json:"average_filled_price"`
	FilledValue          string             `json:"filled_value"`
	CompletionPercentage string             `json:"completion_percentage"`
	CreatedTime          string             `json:"created_time"`
}

// toOrderView renders a record as a Coinbase order, reconstructing the
// order_configuration from the stored fields.
func toOrderView(rec orderRecord) orderView {
	var cfg orderConfiguration
	if rec.OrderType == "MARKET" {
		cfg.MarketIOC = &marketIOCConfig{BaseSize: rec.OrigSize.StringPrec(sizePrec)}
	} else {
		cfg.LimitLimitGTC = &limitGTCConfig{
			BaseSize:   rec.OrigSize.StringPrec(sizePrec),
			LimitPrice: rec.Price.StringPrec(pricePrec),
			PostOnly:   rec.PostOnly,
		}
	}
	return orderView{
		OrderID:              rec.OrderID,
		ClientOrderID:        rec.ClientOrderID,
		ProductID:            rec.ProductID,
		Side:                 rec.Side,
		Status:               rec.Status,
		OrderConfiguration:   cfg,
		FilledSize:           rec.FilledSize.StringPrec(sizePrec),
		AverageFilledPrice:   rec.avgFilledPrice().StringPrec(pricePrec),
		FilledValue:          rec.FilledValue.StringPrec(pricePrec),
		CompletionPercentage: rec.completionPercent().StringPrec(2),
		CreatedTime:          time.UnixMilli(rec.CreatedMs).UTC().Format(time.RFC3339),
	}
}

type historicalBatchResponse struct {
	Orders  []orderView `json:"orders"`
	HasNext bool        `json:"has_next"`
}

// handleHistoricalBatch: GET /api/v3/brokerage/orders/historical/batch
// ?product_id=&order_status=OPEN (SIGNED).
func (s *Server) handleHistoricalBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	if _, err := s.readSignedBody(r); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	engSym := ""
	if productID := q.Get("product_id"); productID != "" {
		e, ok := s.products.Resolve(productID)
		if !ok {
			writeError(w, errInvalidProduct(productID))
			return
		}
		engSym = e
	}

	// This subset tracks OPEN orders in the registry; order_status filtering
	// supports OPEN (the only queryable set without a terminal-order store).
	status := q.Get("order_status")
	if status != "" && status != statusOpen {
		// Other statuses (FILLED/CANCELLED) aren't retained beyond OPEN in this
		// subset; return an empty set rather than an error.
		writeJSON(w, historicalBatchResponse{Orders: []orderView{}, HasNext: false})
		return
	}

	records := s.registry.OpenOrders(engSym)
	out := make([]orderView, 0, len(records))
	for _, rec := range records {
		out = append(out, toOrderView(rec))
	}
	writeJSON(w, historicalBatchResponse{Orders: out, HasNext: false})
}

type singleOrderResponse struct {
	Order orderView `json:"order"`
}

// handleHistoricalOrder: GET /api/v3/brokerage/orders/historical/{order_id}
// (SIGNED).
func (s *Server) handleHistoricalOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	if _, err := s.readSignedBody(r); err != nil {
		writeError(w, err)
		return
	}
	oid := strings.TrimPrefix(r.URL.Path, "/api/v3/brokerage/orders/historical/")
	if oid == "" || strings.Contains(oid, "/") {
		writeError(w, errInvalidArgument("order_id is required"))
		return
	}
	rec, ok := s.registry.snapshot(oid)
	if !ok {
		writeError(w, &apiError{Err: errOrderNotFound, Msg: "order not found: " + oid, status: http.StatusNotFound})
		return
	}
	writeJSON(w, singleOrderResponse{Order: toOrderView(rec)})
}

// accountsResponse is a minimal GET .../accounts shape. Balances are a stub:
// the engine has no ledger, so accounts come from static config (default
// empty).
type accountsResponse struct {
	Accounts []Account `json:"accounts"`
	HasNext  bool      `json:"has_next"`
	Size     int       `json:"size"`
}

// handleAccounts: GET /api/v3/brokerage/accounts (SIGNED).
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errInvalidArgument("method not allowed"))
		return
	}
	if _, err := s.readSignedBody(r); err != nil {
		writeError(w, err)
		return
	}
	accounts := s.accounts
	if accounts == nil {
		accounts = []Account{}
	}
	writeJSON(w, accountsResponse{Accounts: accounts, HasNext: false, Size: len(accounts)})
}

// --- param helpers ---

func parseSide(s string) (orderbook.Side, error) {
	switch s {
	case "BUY":
		return orderbook.SideBuy, nil
	case "SELL":
		return orderbook.SideSell, nil
	case "":
		return "", &apiError{Err: errInvalidSideString, Msg: "side is required", status: http.StatusBadRequest}
	default:
		return "", &apiError{Err: errInvalidSideString, Msg: "invalid side: " + s, status: http.StatusBadRequest}
	}
}

// parseDecimalField parses a Coinbase decimal string field. It rejects empty,
// malformed, and non-finite input.
//
// We deliberately parse the exact decimal string only and do NOT fall back to
// strconv.ParseFloat: ParseFloat accepts "NaN"/"Inf" (which panic
// decimal.FromFloat) and huge exponents (which overflow the 128-bit range and
// panic), turning trivially-crafted input into a handler panic. Coinbase
// clients send plain decimal strings, which decimal.Parse handles.
func parseDecimalField(s, name string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, errInvalidArgument(name + " is required")
	}
	d, err := decimal.Parse(s)
	if err != nil {
		return decimal.Zero, errInvalidArgument("invalid " + name + ": " + s)
	}
	return d, nil
}
