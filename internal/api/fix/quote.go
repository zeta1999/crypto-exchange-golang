package fix

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Quoter is the dealer side of a quote-driven order flow (flow exotics). It is
// deliberately tiny: given an engine instrument and a quantity, return the
// executable bid/ask pair, and say whether an instrument is quote-only (no
// direct book entry). The FIX acceptor stays generic — pricing (saga plugin,
// in-process MC, a remote dealer) is wired in from main via SetQuoter.
type Quoter interface {
	// Quote returns executable bid/ask for the instrument at qty.
	Quote(ctx context.Context, engineSymbol string, qty decimal.Decimal) (bid, offer decimal.Decimal, err error)
	// IsExotic reports whether orders on the instrument must go through the
	// quote flow (a non-quoted NewOrderSingle is answered with a Quote, not
	// traded).
	IsExotic(engineSymbol string) bool
}

// quoteRec is one stored live quote awaiting confirmation.
type quoteRec struct {
	id        string
	sess      *Session
	fixSym    string
	engineSym string
	side      string // requested FIX 54 (1/2)
	qty       decimal.Decimal
	bid       decimal.Decimal
	offer     decimal.Decimal
	expiresAt time.Time
	taken     bool
}

// maxQuotes caps the stored-quote table (the acceptor prunes expired entries
// opportunistically on every quote op; this is a backstop for a runaway peer).
const maxQuotes = 4096

// defaultQuoteTTL applies when no TTL was configured.
const defaultQuoteTTL = 5 * time.Minute

// SetQuoter enables quote-driven (OTC) order entry on this server. The quoter
// is consulted for: QuoteRequest (z) → Quote (S); NewOrderSingle with
// QuotedOrderFlag=Y (692) → executed at the stored quote price; and a plain
// NewOrderSingle on an exotic instrument → answered with a fresh Quote
// instead of being placed.
func (s *Server) SetQuoter(q Quoter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quoter = q
}

// quote executes the OTC flow for one request: price via the quoter, store,
// reply with a Quote (S). quoteReqID is the peer's 131 ("" when the quote was
// auto-generated from a NewOrderSingle).
func (s *Server) quote(sess *Session, quoteReqID, clOrdID, fixSym, engSym, side, qtyStr string, qty decimal.Decimal) {
	if s.quoter == nil {
		s.reject(sess, clOrdID, "", fixSym, side, "no quoter configured")
		return
	}
	now := s.now()
	bid, offer, err := s.quoter.Quote(context.Background(), engSym, qty)
	if err != nil {
		s.reject(sess, clOrdID, "", fixSym, side, "quote failed: "+err.Error())
		return
	}
	if bid.Sign() <= 0 || offer.Sign() <= 0 || bid.Gt(offer) {
		s.reject(sess, clOrdID, "", fixSym, side, fmt.Sprintf("bad quote bid=%s offer=%s", bid, offer))
		return
	}

	s.mu.Lock()
	quoteID := strconv.FormatInt(atomic.AddInt64(&s.quoteSeq, 1), 10)
	rec := &quoteRec{
		id: quoteID, sess: sess, fixSym: fixSym, engineSym: engSym, side: side,
		qty: qty, bid: bid, offer: offer, expiresAt: now.Add(s.quoteTTL),
	}
	s.pruneLocked(now)
	if len(s.quotes) >= maxQuotes {
		s.mu.Unlock()
		s.reject(sess, clOrdID, "", fixSym, side, "quote table full")
		return
	}
	s.quotes[quoteID] = rec
	s.mu.Unlock()

	q := NewMessage(MsgQuote)
	q.Set(TagQuoteID, quoteID)
	if quoteReqID != "" {
		q.Set(TagQuoteReqID, quoteReqID)
	}
	if clOrdID != "" {
		q.Set(TagClOrdID, clOrdID)
	}
	q.Set(TagSymbol, fixSym)
	q.Set(TagSide, side)
	q.Set(TagOrderQty, qtyStr)
	q.Set(TagBidPx, fmtFloat(bid))
	q.Set(TagOfferPx, fmtFloat(offer))
	q.Set(TagBidSize, qtyStr)
	q.Set(TagOfferSize, qtyStr)
	q.Set(TagTransactTime, now.UTC().Format(fixTimeLayout))
	if clOrdID == "" {
		// Auto-quote generated from order interest: tell the peer to confirm
		// via NewOrderSingle with QuotedOrderFlag=Y and OrderID=117.
		q.Set(TagText, "confirm: NewOrderSingle with QuotedOrderFlag=Y (692) and OrderID (37) = QuoteID (117)")
	}
	_ = sess.Send(q)
}

// pruneLocked drops expired/taken quotes (caller holds s.mu).
func (s *Server) pruneLocked(now time.Time) {
	for id, q := range s.quotes {
		if q.taken || now.After(q.expiresAt) {
			delete(s.quotes, id)
		}
	}
}

// executeQuoted consumes a stored quote and places its order at the quoted
// price: take OFFER for a buy (side 1), take BID for a sell (side 2). A quote
// is usable at most once, only unexpired, only by the session that got it.
func (s *Server) executeQuoted(sess *Session, m *Message, clOrdID, fixSym, engSym, side string, qty decimal.Decimal) {
	quoteID, _ := m.Get(TagOrderID) // confirmations carry the QuoteID in 37
	var px decimal.Decimal
	var ok bool
	s.mu.Lock()
	if rec, found := s.quotes[quoteID]; found {
		if sess != rec.sess {
			s.reject(sess, clOrdID, "", fixSym, side, "quote "+quoteID+" belongs to another session")
			s.mu.Unlock()
			return
		}
		if s.now().After(rec.expiresAt) {
			delete(s.quotes, quoteID)
			s.reject(sess, clOrdID, "", fixSym, side, "quote "+quoteID+" expired")
			s.mu.Unlock()
			return
		}
		if rec.taken {
			s.reject(sess, clOrdID, "", fixSym, side, "quote "+quoteID+" already executed")
			s.mu.Unlock()
			return
		}
		if rec.fixSym != fixSym {
			s.reject(sess, clOrdID, "", fixSym, side, "quote "+quoteID+" is for "+rec.fixSym)
			s.mu.Unlock()
			return
		}
		rec.taken = true
		if side == "1" {
			px = rec.offer
		} else {
			px = rec.bid
		}
		ok = true
	}
	s.mu.Unlock()
	if !ok {
		s.reject(sess, clOrdID, "", fixSym, side, "unknown quote "+quoteID)
		return
	}

	// Same order-registration + fill-routing machinery as regular orders; the
	// price is the QUOTED one (the client's 44, if any, is ignored).
	// Idempotency (parity with the regular D path): a duplicate ClOrdID on the
	// same session is rejected, never a second order.
	s.mu.Lock()
	if st := s.sessions[sess]; st != nil {
		if _, dup := st.byClOrdID[clOrdID]; dup {
			s.mu.Unlock()
			s.reject(sess, clOrdID, "", fixSym, side, "duplicate ClOrdID "+clOrdID)
			return
		}
	}
	s.mu.Unlock()

	orderID := strconv.FormatInt(atomic.AddInt64(&s.orderSeq, 1), 10)
	engineID := enginePrefix + orderID
	rec := &ordRec{
		sess: sess, clOrdID: clOrdID, orderID: orderID, engineID: engineID,
		fixSymbol: fixSym, engineSym: engSym, side: side, ordType: "2",
		price: fmtFloat(px), orderQty: qty,
	}
	s.mu.Lock()
	if st := s.sessions[sess]; st != nil {
		st.byClOrdID[clOrdID] = rec
	}
	s.byEngine[engineID] = rec
	s.mu.Unlock()

	s.sendExecReport(rec, execTypeNew, ordStatusNew, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, "")

	ord := &orderbook.Order{
		ID: engineID, Instrument: engSym, Volume: qty, Side: engSideFor(side),
		Price:    px,
		Metadata: map[string]string{"fix": "1", "clOrdID": clOrdID, "quoted": quoteID},
	}
	if _, _, err := s.engine.PlaceLimit(context.Background(), ord); err != nil {
		s.finishReject(rec, "engine rejected: "+err.Error())
	}
}

// fmtFloat renders a decimal deterministically for the wire (FIX is string-based).
func fmtFloat(d decimal.Decimal) string { return d.String() }

// engSideFor maps FIX side 1(B)/2(S) to the engine side; panics on garbage —
// only called after fixSideToEngine validation in the D path.
func engSideFor(fixSide string) orderbook.Side {
	if e, ok := fixSideToEngine(fixSide); ok {
		return e
	}
	return orderbook.SideBuy
}
