package binance

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zeta1999/crypto-exchange-golang/internal/optmarket"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// --- option order entry on the EAPI surface ---
//
// A listed option trades in ONE ledger asset — its own contract,
// "OTC:<symbol>" — against the QUOTE asset: a BUY mints contract units and
// pays quote; a SELL redeems contract units and receives quote. Option orders
// are plain limit orders on the SAME engine book as the spot pairs (the
// exchange registers the option symbols on the book at boot), so matching,
// fills, the WAL, and the existing WS executionReport flow are reused; only
// the ledger settlement shape differs (settleOptionFill, because
// settleFill would misparse the 4-token option symbol as a spot pair).
//
// EAPI option orders are LIMIT-only (GTC): a European cash-settled contract
// has no "market" semantics to fake. The limit price must sit inside the
// contract's mark band (highPriceLimit/lowPriceLimit); a duplicate
// newClientOrderId is -2010 "Duplicate order sent." (CR-5 idempotency).
//
// At the settlement instant the expiry sweep (SettleOptions) cancels every
// resting option order and settles CASH in the quote asset at the instrument's
// settlement value — vanilla: its own intrinsic off the index rule; exotic:
// its own payoff's fresh terminal value (never a vanilla leg).

// isOptionSymbol reports whether an engine symbol routes to the options
// book: EAPI option symbols are 4 dashed tokens (BTC-261231-50000-C,
// BTC-261231-50000-BAR); spot engine symbols are two (BTC-USD).
func isOptionBookSymbol(eng string) bool {
	return strings.Count(eng, "-") >= 3
}

// isOptionSym is the ROUTING check for a fill or cancel: does this engine
// symbol belong to a LISTED option? The precise check (the options market's
// listing) when an options surface is wired, so a hypothetical spot symbol
// with three or more hyphens can never be mis-routed to the contract ledger;
// the dash-count heuristic only as the no-options fallback.
func (s *Server) isOptionSym(eng string) bool {
	if om := s.optMarket; om != nil {
		return om.IsListed(eng)
	}
	return isOptionBookSymbol(eng)
}

// eapiFill is one element of the option place-order response's fills array.
type eapiFill struct {
	Symbol   string `json:"symbol"`
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
	Side     string `json:"side"`
}

// optionPlaceResponse is the POST /eapi/v1/order response.
type optionPlaceResponse struct {
	OptionOrderStatus
	Fills []eapiFill `json:"fills"`
}

// OptionOrderStatus is the EAPI current state of one option order.
type OptionOrderStatus struct {
	Symbol             string `json:"symbol"`
	OrderID            int64  `json:"orderId"`
	ClientOrderID      string `json:"clientOrderId"`
	Side               string `json:"side"`
	Type               string `json:"type"`
	Price              string `json:"price"`
	Quantity           string `json:"quantity"`
	ExecutedQuantity   string `json:"executedQuantity"`
	CumulativeQuoteQty string `json:"cumulativeQuoteQty"`
	AvgPrice           string `json:"avgPrice"`
	Status             string `json:"status"` // NEW | PARTIALLY_FILLED | FILLED | CANCELED
}

// optionOrderView renders one option order record.
func (r *Registry) optionOrderView(rec *orderRecord) OptionOrderStatus {
	avg := "0"
	if rec.ExecutedQty.Sign() > 0 {
		avg = rec.CummulativeQuoteQty.Div(rec.ExecutedQty).StringPrec(pricePrec)
	}
	return OptionOrderStatus{
		Symbol:             rec.BinanceSymbol,
		OrderID:            rec.OrderID,
		ClientOrderID:      rec.ClientOrderID,
		Side:               rec.Side,
		Type:               rec.Type,
		Price:              rec.Price.StringPrec(pricePrec),
		Quantity:           rec.OrigQty.StringPrec(qtyPrec),
		ExecutedQuantity:   rec.ExecutedQty.StringPrec(qtyPrec),
		CumulativeQuoteQty: rec.CummulativeQuoteQty.StringPrec(pricePrec),
		AvgPrice:           avg,
		Status:             rec.Status,
	}
}

// optionOrderQuery looks an option order up by orderId or origClientOrderId;
// when a symbol qualifier is given it must agree with the record.
func (s *Server) optionOrderQuery(sym string, q url.Values) (*orderRecord, bool) {
	var rec *orderRecord
	if idStr := q.Get("orderId"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		rec, _ = s.registry.Get(id)
	} else if coid := q.Get("origClientOrderId"); coid != "" {
		rec, _ = s.registry.GetByClientOrderID(coid)
	}
	if rec == nil || !isOptionBookSymbol(rec.EngineSymbol) {
		return nil, false
	}
	if sym != "" && rec.EngineSymbol != sym {
		return nil, false
	}
	return rec, true
}

// handleOptionOrder: /eapi/v1/order multiplexes POST (place), GET (query one
// by orderId|origClientOrderId) and DELETE (cancel), ?symbol= required for the
// GET/DELETE variants.
func (s *Server) handleOptionOrder(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.optionPlaceOrder(w, r)
	case http.MethodGet:
		s.optionQueryOrder(w, r)
	case http.MethodDelete:
		s.optionCancelOrder(w, r)
	default:
		writeError(w, errIllegalParam("method"))
	}
}

// optionPlaceOrder: POST /eapi/v1/order (SIGNED). Places a LIMIT (GTC) order
// on a listed option. Idempotent on newClientOrderId.
func (s *Server) optionPlaceOrder(w http.ResponseWriter, r *http.Request) {
	om := s.optMarket
	if om == nil {
		writeError(w, errInvalidSymbol())
		return
	}
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	_ = r.ParseForm()
	get := func(k string) string {
		if v := r.URL.Query().Get(k); v != "" {
			return v
		}
		return r.PostFormValue(k)
	}
	sym := get("symbol")
	if !om.IsListed(sym) {
		writeError(w, errInvalidSymbol())
		return
	}
	// A contract stops being tradeable AT its settlement instant (and forever
	// after, once settled): an entry placed past expiry would either rest
	// unsettled (its position stranded un-cashed) or cross stale liquidity
	// that no settlement will ever cash out.
	if om.IsSettled(sym) {
		writeError(w, errContractSettled())
		return
	}
	if exp, ok := om.ExpiryOf(sym); ok && !s.now().Before(exp) {
		writeError(w, errContractSettled())
		return
	}
	var side orderbook.Side
	switch get("side") {
	case "BUY":
		side = orderbook.SideBuy
	case "SELL":
		side = orderbook.SideSell
	default:
		writeError(w, errIllegalParam("side"))
		return
	}
	if get("type") != "LIMIT" {
		writeError(w, errIllegalParam("type"))
		return
	}
	qty, err := parseDecimalParam(get("quantity"), "quantity")
	if err != nil {
		writeError(w, err)
		return
	}
	if qty.Sign() <= 0 {
		writeError(w, errIllegalParam("quantity"))
		return
	}
	price, err := parseDecimalParam(get("price"), "price")
	if err != nil {
		writeError(w, err)
		return
	}
	if price.Sign() <= 0 {
		writeError(w, errIllegalParam("price"))
		return
	}
	// Price band: the limit must sit inside the mark's band (EAPI
	// highPriceLimit/lowPriceLimit discipline). When no mark is available
	// (underlying has no index yet) there is no band to enforce.
	if mk, merr := om.Mark(sym); merr == nil {
		p := price.Float64()
		if (mk.HighPriceLimit > 0 && p > mk.HighPriceLimit) ||
			(mk.LowPriceLimit > 0 && p < mk.LowPriceLimit) {
			writeError(w, errPriceNotWithinBands())
			return
		}
	}
	tif := get("timeInForce")
	if tif == "" {
		tif = "GTC"
	}
	sideStr := "BUY"
	if side == orderbook.SideSell {
		sideStr = "SELL"
	}
	// Place-time idempotency on newClientOrderId (CR-5's RecordUnique): a
	// duplicate id is -2010, never a second resting order. The id is claimed
	// BEFORE the fund reservation, so a rejected place frees it for a retry.
	rec, err := s.registry.RecordUnique(sym, sym, sideStr, "LIMIT", tif, price, qty, get("newClientOrderId"))
	if err != nil {
		writeError(w, errDuplicateOrder())
		return
	}
	quote, contractAsset, _ := om.QuoteAndAsset(sym)
	var lockAsset string
	var lockAmt decimal.Decimal
	if s.ledger != nil {
		if side == orderbook.SideBuy {
			// A contract BUY reserves the QUOTE it may spend (price·qty).
			lockAsset, lockAmt = quote, price.Mul(qty)
		} else {
			// A contract SELL reserves the CONTRACT UNITS it redeems (qty).
			lockAsset, lockAmt = contractAsset, qty
		}
		if !s.ledger.Lock(lockAsset, lockAmt) {
			s.registry.Remove(rec.OrderID)
			writeError(w, errInsufficientBalance())
			return
		}
	}
	// Ack before placement so user-data subscribers see NEW ahead of any
	// synchronous TRADE the book hook fires inside PlaceLimit.
	s.emitExecutionReport(rec.EngineID, execTypeNew, decimal.Zero, decimal.Zero)
	ord := &orderbook.Order{
		ID:         rec.EngineID,
		Instrument: sym,
		Price:      price,
		Volume:     qty,
		Side:       side,
		Metadata:   map[string]string{"api": "eapi", "clientOrderId": rec.ClientOrderID},
	}
	trades, _, err := s.engine.PlaceLimit(r.Context(), ord)
	if err != nil {
		s.registry.Remove(rec.OrderID)
		if s.ledger != nil {
			// A place that failed reserves nothing (no fills happened), so
			// release the full reservation.
			if side == orderbook.SideBuy {
				s.ledger.Unlock(quote, price.Mul(qty))
			} else {
				s.ledger.Unlock(contractAsset, qty)
			}
		}
		writeError(w, errInternal(err.Error()))
		return
	}
	fills := make([]eapiFill, 0, len(trades))
	for _, t := range trades {
		fills = append(fills, eapiFill{
			Symbol:   sym,
			Price:    t.Price.StringPrec(pricePrec),
			Quantity: t.Volume.StringPrec(qtyPrec),
			Side:     sideStr,
		})
	}
	s.sleepAck(r.Context())
	snap, _ := s.registry.snapshot(rec.OrderID)
	writeJSON(w, optionPlaceResponse{
		OptionOrderStatus: s.registry.optionOrderView(&snap),
		Fills:             fills,
	})
}

// optionCancelOrder: DELETE /eapi/v1/order (SIGNED) by orderId |
// origClientOrderId; ?symbol= is a required qualifier.
func (s *Server) optionCancelOrder(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	symC := q.Get("symbol")
	if symC == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	rec, ok := s.optionOrderQuery(symC, q)
	if !ok {
		writeError(w, errUnknownOrder())
		return
	}
	if _, err := s.engine.CancelOrder(r.Context(), rec.EngineSymbol, rec.EngineID); err != nil {
		cur, _ := s.registry.snapshot(rec.OrderID)
		if cur.Status == statusFilled || cur.Status == statusCanceled {
			writeJSON(w, s.registry.optionOrderView(&cur))
			return
		}
		writeError(w, errUnknownOrder())
		return
	}
	s.registry.MarkCanceled(rec.OrderID)
	cur, _ := s.registry.snapshot(rec.OrderID)
	writeJSON(w, s.registry.optionOrderView(&cur))
}

// optionQueryOrder: GET /eapi/v1/order (SIGNED) — one option order's current
// state (open or terminal); ?symbol= is a required qualifier.
func (s *Server) optionQueryOrder(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	symC := q.Get("symbol")
	if symC == "" {
		writeError(w, errMandatoryParam("symbol"))
		return
	}
	rec, ok := s.optionOrderQuery(symC, q)
	if !ok {
		writeError(w, errUnknownOrder())
		return
	}
	// Locked copy: the record is live (the book may be filling it right now).
	cur, _ := s.registry.snapshot(rec.OrderID)
	writeJSON(w, s.registry.optionOrderView(&cur))
}

// optionOpenOrders: GET /eapi/v1/orders (SIGNED) — option orders still working
// (NEW / PARTIALLY_FILLED); ?symbol= optional.
func (s *Server) optionOpenOrders(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Verify(r); err != nil {
		writeError(w, err)
		return
	}
	sym := r.URL.Query().Get("symbol")
	out := make([]OptionOrderStatus, 0)
	for _, rec := range s.registry.OpenOrders(sym) {
		if !isOptionBookSymbol(rec.EngineSymbol) {
			continue
		}
		rr := rec
		out = append(out, s.registry.optionOrderView(&rr))
	}
	writeJSON(w, out)
}

// settleOptionFill settles one option fill in the ledger. The option's
// EngineSymbol is the EAPI symbol (NOT a spot "BASE-QUOTE" pair), so — unlike
// settleFill — the ledger's base/quote are (contractAsset, quote-asset): a
// LIMIT BUY's reservation (rec.Price) is released and the actual cost charged
// from quote, crediting contract units; the SELL mirrors it. MARKET orders are
// not accepted, so reservedPrice is always the order's limit price.
func (s *Server) settleOptionFill(engineID string, isBuy bool, qty, fillPrice decimal.Decimal) {
	rec, ok := s.registry.getByEngine(engineID)
	if !ok || s.ledger == nil {
		return
	}
	quote := "USDT"
	contractAsset := "OTC:" + rec.EngineSymbol
	if om := s.optMarket; om != nil {
		if q, a, ok2 := om.QuoteAndAsset(rec.EngineSymbol); ok2 {
			quote, contractAsset = q, a
		}
	}
	s.ledger.SettleFill(isBuy, contractAsset, quote, qty, fillPrice, rec.Price)
}

// defaultOptionPositions returns the client's HELD contracts in `sym` (the
// ledger's free OTC balance) as an EXACT decimal — the settlement cash path
// must not round-trip the held quantity through float64. The single-account
// emulated model has no net-short positions: a SELL cannot exceed the held
// balance (it is rejected past it), so the free OTC balance is the full long
// position. The venue's resting synthetic liquidity lives in the order BOOK,
// not the ledger, so it never pollutes this number.
func (s *Server) defaultOptionPositions(sym string) decimal.Decimal {
	if s.ledger == nil || s.optMarket == nil {
		return decimal.Zero
	}
	_, asset, ok := s.optMarket.QuoteAndAsset(sym)
	if !ok {
		return decimal.Zero
	}
	return s.ledger.Get(asset).Free
}

// SettleOptions runs ONE expiry sweep against the options book: settles every
// contract expired on this pass — canceling its resting orders (their
// reservations return to free) and cashing the client's position in the quote
// asset at the settlement value (vanilla: intrinsic off the index rule; exotic:
// the fresh payoff terminal via the attached pricer), then burning the expired
// contract asset. Within the process a settled contract never settles twice;
// a contract with no index / an unpricable payoff is left unsettled and retried.
func (s *Server) SettleOptions(ctx context.Context) []optmarket.Settlement {
	om := s.optMarket
	if om == nil {
		return nil
	}
	// Position source: the client's LEDGER holding (the single-account model:
	// the edge's ledger IS the client's account). Cash-settle hook: the
	// production default (settleOptionCash) — credits the QUOTE asset and
	// burns the contract. Sweep diagnostics go to the process log once per
	// message (a broken exotic retries every tick; reported once, not 1 Hz).
	var hook func(rec optmarket.Settlement, longQty decimal.Decimal)
	if s.ledger != nil {
		hook = s.settleOptionCash
	}
	return om.Settle(ctx, &optmarket.Settler{
		Matcher: optSettlerMatcher{book: s.bookRef},
		Logf:    s.settlePrintf,
	}, s.defaultOptionPositions, hook)
}

// settlePrintf logs a sweep diagnostic ONCE per message-prefix per process:
// a permanently-broken exotic (or a stuck cancel) is reported, not chatty —
// the sweep keeps retrying it every tick silently.
func (s *Server) settlePrintf(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	key := splitFields2(line)
	s.settleLogMu.Lock()
	defer s.settleLogMu.Unlock()
	if _, seen := s.settleLogSeen[key]; seen {
		return
	}
	s.settleLogSeen[key] = struct{}{}
	log.Printf("options settle: %s", line)
}

// splitFields2 keys a log line by its first two fields (symbol + verb) so one
// flapping exotic's line is not re-logged every tick.
func splitFields2(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return line
	}
	return f[0] + " " + f[1]
}

// settleOptionCash settles one settled contract for the client's held units:
// credit the QUOTE asset with longQty·PerUnit (PerUnit already folds the
// contract multiplier), then BURN the expired contract units (expired paper
// is not a position; it must not linger in /account). In the single-account
// model the long cannot exceed free (a sell is rejected past it), so credit
// then burn is exact; the clamp in Burn is the belt for a rounding residual.
func (s *Server) settleOptionCash(rec optmarket.Settlement, longQty decimal.Decimal) {
	if s.ledger == nil || longQty.IsZero() {
		return
	}
	quote, asset, ok := s.optMarket.QuoteAndAsset(rec.Symbol)
	if !ok {
		return
	}
	// Credit the exact held quantity times the settlement value. longQty stays
	// an exact decimal end-to-end (no float round-trip of the held contracts);
	// only PerUnit — the observed index/pricer settlement print — is float64.
	s.ledger.Credit(quote, longQty.Mul(decimal.FromFloat(rec.PerUnit)))
	s.ledger.Burn(asset, longQty)
}

// optSettlerMatcher adapts the engine book to the optmarket sweep's Matcher
// (list the rests, cancel them).
type optSettlerMatcher struct {
	book *orderbook.OrderBook
}

func (o optSettlerMatcher) CancelOrder(_ context.Context, instrument, orderID string) (*orderbook.Order, error) {
	if o.book == nil {
		return nil, orderbook.ErrOrderNotFound
	}
	return o.book.CancelOrder(instrument, orderID)
}

func (o optSettlerMatcher) OpenOrders(instrument string) []*orderbook.Order {
	if o.book == nil {
		return nil
	}
	return o.book.OpenOrders(instrument)
}

var _ = http.MethodPost // the /eapi/v1/order multiplexing handler
