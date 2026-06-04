package fix

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Engine is the subset of the matching engine the FIX edge consumes (identical to
// the REST edge's view — the SAME engine backs both).
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
}

// SymbolResolver maps a FIX Symbol (55) to an engine instrument, false if unknown.
type SymbolResolver func(fixSymbol string) (engineSymbol string, ok bool)

// enginePrefix tags engine order IDs minted by the FIX edge, so the shared book
// hook can tell a FIX order from a REST order.
const enginePrefix = "fix:"

// ordRec tracks one live FIX order for fill routing + cancel + idempotency.
type ordRec struct {
	sess      *Session
	clOrdID   string
	orderID   string // exchange OrderID (FIX 37)
	engineID  string // orderbook order id (enginePrefix + orderID)
	fixSymbol string
	engineSym string
	side      string // FIX 54
	ordType   string // FIX 40
	price     string // FIX 44 (limit)
	orderQty  decimal.Decimal
	cumQty    decimal.Decimal
	avgNum    decimal.Decimal // sum(px*qty) for AvgPx
	done      bool
}

// sessState is per-connection FIX state: ClOrdID idempotency + MD subscriptions.
type sessState struct {
	byClOrdID map[string]*ordRec
	subs      map[string]string // fixSymbol -> MDReqID (active subscriptions)
}

// Server is the FIX 4.4 acceptor. It is the shared Application across all
// sessions and owns order/fill routing against the engine.
type Server struct {
	engine       Engine
	resolve      SymbolResolver
	dict         *Dictionary
	now          func() time.Time
	senderCompID string
	depth        int // default market-data depth

	mu       sync.Mutex
	sessions map[*Session]*sessState
	byEngine map[string]*ordRec // engineID -> order (cross-session fill routing)
	orderSeq int64
	execSeq  int64
}

// NewServer builds the FIX acceptor. senderCompID is the acceptor's CompID.
func NewServer(engine Engine, resolve SymbolResolver, senderCompID string, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{
		engine: engine, resolve: resolve, dict: NewDictionary(),
		now: now, senderCompID: senderCompID, depth: 20,
		sessions: map[*Session]*sessState{},
		byEngine: map[string]*ordRec{},
	}
}

// Accept runs a FIX session over conn until it closes. Blocking; call per conn.
func (s *Server) Accept(conn net.Conn) error {
	sess := NewSession(conn, s.senderCompID, s, s.dict, s.now)
	return sess.Run()
}

// ListenAndServe accepts FIX connections on addr until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go func() { _ = s.Accept(conn) }()
	}
}

// AttachHooks registers the book hook that routes async FILLS to FIX sessions.
// Cancels are reported synchronously by the cancel handler, so only "trade" is
// handled here (mirrors the REST edge's executionReport-on-fill path).
func (s *Server) AttachHooks(book *orderbook.OrderBook) {
	book.RegisterHook(func(evt string, data interface{}) {
		if evt != "trade" {
			return
		}
		t, ok := data.(*orderbook.Trade)
		if !ok {
			return
		}
		for _, id := range []string{t.BuyOrderID, t.SellOrderID} {
			s.onFill(id, t)
		}
		s.fanoutTrade(t)
	})
}

// ---- Application interface ----

func (s *Server) OnLogon(sess *Session) {
	s.mu.Lock()
	s.sessions[sess] = &sessState{byClOrdID: map[string]*ordRec{}, subs: map[string]string{}}
	s.mu.Unlock()
}

func (s *Server) OnLogout(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.sessions[sess]; ok {
		for _, rec := range st.byClOrdID {
			delete(s.byEngine, rec.engineID)
		}
		delete(s.sessions, sess)
	}
}

func (s *Server) OnNewOrderSingle(sess *Session, m *Message) {
	clOrdID, _ := m.Get(TagClOrdID)
	fixSym, _ := m.Get(TagSymbol)
	side, _ := m.Get(TagSide)
	ordType, _ := m.Get(TagOrdType)
	qtyStr, _ := m.Get(TagOrderQty)
	pxStr, _ := m.Get(TagPrice)

	engSym, ok := s.resolve(fixSym)
	if !ok {
		s.reject(sess, clOrdID, "", fixSym, side, "unknown symbol "+fixSym)
		return
	}
	engSide, ok := fixSideToEngine(side)
	if !ok {
		s.reject(sess, clOrdID, "", fixSym, side, "bad Side "+side)
		return
	}
	qty, err := decimal.Parse(qtyStr)
	if err != nil || qty.Sign() <= 0 {
		s.reject(sess, clOrdID, "", fixSym, side, "bad OrderQty "+qtyStr)
		return
	}

	// Idempotency: a duplicate ClOrdID on the same session is rejected, never a
	// second resting order (the FIX analogue of CR-5's place-time idempotency).
	s.mu.Lock()
	st := s.sessions[sess]
	if st != nil {
		if _, dup := st.byClOrdID[clOrdID]; dup {
			s.mu.Unlock()
			s.reject(sess, clOrdID, "", fixSym, side, "duplicate ClOrdID "+clOrdID)
			return
		}
	}
	orderID := strconv.FormatInt(atomic.AddInt64(&s.orderSeq, 1), 10)
	engineID := enginePrefix + orderID
	rec := &ordRec{
		sess: sess, clOrdID: clOrdID, orderID: orderID, engineID: engineID,
		fixSymbol: fixSym, engineSym: engSym, side: side, ordType: ordType,
		price: pxStr, orderQty: qty,
	}
	if st != nil {
		st.byClOrdID[clOrdID] = rec
	}
	s.byEngine[engineID] = rec
	s.mu.Unlock()

	// Emit NEW *before* placement so an immediate (taker) fill report follows it.
	// cumQty/avgNum are zero here (nothing has filled yet).
	s.sendExecReport(rec, execTypeNew, ordStatusNew, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, "")

	ord := &orderbook.Order{
		ID: engineID, Instrument: engSym, Volume: qty, Side: engSide,
		Metadata: map[string]string{"fix": "1", "clOrdID": clOrdID},
	}
	if ordType == "1" { // MARKET
		ord.IsMarket = true
		_, _, err = s.engine.PlaceMarket(context.Background(), ord)
	} else { // LIMIT (validated to carry Price)
		px, perr := decimal.Parse(pxStr)
		if perr != nil {
			s.finishReject(rec, "bad Price "+pxStr)
			return
		}
		ord.Price = px
		_, _, err = s.engine.PlaceLimit(context.Background(), ord)
	}
	if err != nil {
		s.finishReject(rec, "engine rejected: "+err.Error())
	}
}

func (s *Server) OnOrderCancelRequest(sess *Session, m *Message) {
	clOrdID, _ := m.Get(TagClOrdID)
	orig, _ := m.Get(TagOrigClOrdID)
	fixSym, _ := m.Get(TagSymbol)

	s.mu.Lock()
	st := s.sessions[sess]
	var rec *ordRec
	if st != nil {
		rec = st.byClOrdID[orig]
	}
	missing := rec == nil || rec.done // read `done` under the lock
	s.mu.Unlock()
	if missing {
		s.cancelReject(sess, clOrdID, orig, fixSym, "unknown or completed order")
		return
	}

	if _, err := s.engine.CancelOrder(context.Background(), rec.engineSym, rec.engineID); err != nil {
		// Lost a race to a concurrent fill (the order is already gone) — report it.
		s.cancelReject(sess, clOrdID, orig, fixSym, "engine: "+err.Error())
		return
	}
	s.mu.Lock()
	rec.done = true
	cum, avgNum := rec.cumQty, rec.avgNum // snapshot under the lock
	delete(s.byEngine, rec.engineID)
	s.mu.Unlock()
	// Canceled ExecutionReport carries the cancel's ClOrdID + the OrigClOrdID.
	rep := s.baseExecReport(rec, execTypeCanceled, ordStatusCanceled, cum, avgNum)
	rep.Set(TagClOrdID, clOrdID)
	rep.Set(TagOrigClOrdID, orig)
	_ = sess.Send(rep)
}

// OnOrderCancelReplace cancels the original order, then places the replacement
// as a fresh order — which is acknowledged with an ExecType=New ExecutionReport
// (a first-cut cancel/replace; a single ExecType=Replaced report is a follow-up).
// Same idempotent ClOrdID rules apply to the replacement's ClOrdID.
func (s *Server) OnOrderCancelReplace(sess *Session, m *Message) {
	orig, _ := m.Get(TagOrigClOrdID)
	s.mu.Lock()
	st := s.sessions[sess]
	var rec *ordRec
	if st != nil {
		rec = st.byClOrdID[orig]
	}
	missing := rec == nil || rec.done // read `done` under the lock
	s.mu.Unlock()
	if missing {
		clOrdID, _ := m.Get(TagClOrdID)
		fixSym, _ := m.Get(TagSymbol)
		s.cancelReject(sess, clOrdID, orig, fixSym, "unknown or completed order")
		return
	}
	if _, err := s.engine.CancelOrder(context.Background(), rec.engineSym, rec.engineID); err != nil {
		clOrdID, _ := m.Get(TagClOrdID)
		fixSym, _ := m.Get(TagSymbol)
		s.cancelReject(sess, clOrdID, orig, fixSym, "engine: "+err.Error())
		return
	}
	s.mu.Lock()
	rec.done = true
	delete(s.byEngine, rec.engineID)
	s.mu.Unlock()
	// Place the replacement as a fresh NewOrderSingle (new ClOrdID/qty/price).
	s.OnNewOrderSingle(sess, m)
}

// OnMarketDataRequest answers a snapshot (W) for each requested symbol and, for a
// subscribe (263=1), registers the symbols so trades fan out incremental (X).
func (s *Server) OnMarketDataRequest(sess *Session, m *Message) {
	reqID, _ := m.Get(TagMDReqID)
	subType, _ := m.Get(TagSubReqType)
	depth := s.depth
	if d, ok := m.GetInt(TagMarketDepth); ok && d > 0 {
		depth = d
	}
	// Collect requested FIX symbols (the NoRelatedSym group's 55 fields).
	var symbols []string
	for _, f := range m.Fields {
		if f.Tag == TagSymbol {
			symbols = append(symbols, f.Value)
		}
	}
	for _, fixSym := range symbols {
		engSym, ok := s.resolve(fixSym)
		if !ok {
			s.mdReject(sess, reqID, "unknown symbol "+fixSym)
			continue
		}
		snap, err := s.engine.Snapshot(engSym)
		if err != nil {
			s.mdReject(sess, reqID, "no book for "+fixSym)
			continue
		}
		_ = sess.Send(s.snapshotMsg(reqID, fixSym, snap, depth))
		if subType == "1" { // snapshot + updates
			s.mu.Lock()
			if st := s.sessions[sess]; st != nil {
				st.subs[fixSym] = reqID
			}
			s.mu.Unlock()
		}
	}
}

// ---- fill routing ----

func (s *Server) onFill(engineID string, t *orderbook.Trade) {
	s.mu.Lock()
	rec, ok := s.byEngine[engineID]
	if !ok {
		s.mu.Unlock()
		return
	}
	rec.cumQty = rec.cumQty.Add(t.Volume)
	rec.avgNum = rec.avgNum.Add(t.Volume.Mul(t.Price))
	cum, avgNum := rec.cumQty, rec.avgNum // snapshot mutable state under the lock
	filled := rec.orderQty.Sub(cum).Sign() <= 0
	if filled {
		rec.done = true
		delete(s.byEngine, engineID)
	}
	s.mu.Unlock()

	status := ordStatusPartial
	if filled {
		status = ordStatusFilled
	}
	s.sendExecReport(rec, execTypeTrade, status, cum, avgNum, t.Volume, t.Price, "")
}

// ---- ExecutionReport builders ----

const (
	execTypeNew      = "0"
	execTypeCanceled = "4"
	execTypeReplaced = "5"
	execTypeRejected = "8"
	execTypeTrade    = "F"

	ordStatusNew      = "0"
	ordStatusPartial  = "1"
	ordStatusFilled   = "2"
	ordStatusCanceled = "4"
	ordStatusRejected = "8"
)

// baseExecReport builds an ExecutionReport from the order's IMMUTABLE fields plus
// a caller-supplied snapshot of the mutable cumulative state (cumQty/avgNum), so
// it never reads `rec.cumQty`/`rec.avgNum`/`rec.done` — those are written under
// s.mu by onFill on another goroutine, so reading them here unsynchronized would
// race a concurrent fill. Leaves is derived from execType (a terminal Canceled/
// Rejected/Replaced report carries 0).
func (s *Server) baseExecReport(rec *ordRec, execType, ordStatus string, cumQty, avgNum decimal.Decimal) *Message {
	var leaves decimal.Decimal
	switch execType {
	case execTypeCanceled, execTypeRejected, execTypeReplaced:
		leaves = decimal.Zero
	default: // New, Trade
		leaves = rec.orderQty.Sub(cumQty)
		if leaves.Sign() < 0 {
			leaves = decimal.Zero
		}
	}
	avg := decimal.Zero
	if cumQty.Sign() > 0 {
		avg = avgNum.Div(cumQty)
	}
	rep := NewMessage(MsgExecutionReport)
	rep.Set(TagOrderID, rec.orderID)
	rep.Set(TagClOrdID, rec.clOrdID)
	rep.Set(TagExecID, strconv.FormatInt(atomic.AddInt64(&s.execSeq, 1), 10))
	rep.Set(TagExecType, execType)
	rep.Set(TagOrdStatus, ordStatus)
	rep.Set(TagSymbol, rec.fixSymbol)
	rep.Set(TagSide, rec.side)
	rep.Set(TagOrderQty, rec.orderQty.String())
	rep.Set(TagOrdType, rec.ordType)
	if rec.ordType == "2" && rec.price != "" {
		rep.Set(TagPrice, rec.price)
	}
	rep.Set(TagLeavesQty, leaves.String())
	rep.Set(TagCumQty, cumQty.String())
	rep.Set(TagAvgPx, avg.String())
	rep.Set(TagTransactTime, s.now().UTC().Format(fixTimeLayout))
	return rep
}

// sendExecReport sends a report from a snapshot of (cumQty, avgNum), optionally
// carrying last-fill qty/px (for trades).
func (s *Server) sendExecReport(rec *ordRec, execType, ordStatus string, cumQty, avgNum, lastQty, lastPx decimal.Decimal, text string) {
	rep := s.baseExecReport(rec, execType, ordStatus, cumQty, avgNum)
	if execType == execTypeTrade {
		rep.Set(TagLastQty, lastQty.String())
		rep.Set(TagLastPx, lastPx.String())
	}
	if text != "" {
		rep.Set(TagText, text)
	}
	_ = rec.sess.Send(rep)
}

// reject emits a Rejected ExecutionReport for an order that never made the book.
func (s *Server) reject(sess *Session, clOrdID, orderID, fixSym, side, text string) {
	rep := NewMessage(MsgExecutionReport)
	rep.Set(TagOrderID, orderID)
	rep.Set(TagClOrdID, clOrdID)
	rep.Set(TagExecID, strconv.FormatInt(atomic.AddInt64(&s.execSeq, 1), 10))
	rep.Set(TagExecType, execTypeRejected)
	rep.Set(TagOrdStatus, ordStatusRejected)
	rep.Set(TagSymbol, fixSym)
	rep.Set(TagSide, side)
	rep.Set(TagText, text)
	rep.Set(TagTransactTime, s.now().UTC().Format(fixTimeLayout))
	_ = sess.Send(rep)
}

// finishReject backs out a partially-registered order then rejects it.
func (s *Server) finishReject(rec *ordRec, text string) {
	s.mu.Lock()
	rec.done = true
	delete(s.byEngine, rec.engineID)
	if st := s.sessions[rec.sess]; st != nil {
		delete(st.byClOrdID, rec.clOrdID)
	}
	s.mu.Unlock()
	s.sendExecReport(rec, execTypeRejected, ordStatusRejected, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, text)
}

func (s *Server) cancelReject(sess *Session, clOrdID, orig, fixSym, text string) {
	rj := NewMessage(MsgOrderCancelReject)
	rj.Set(TagClOrdID, clOrdID)
	rj.Set(TagOrigClOrdID, orig)
	rj.Set(TagOrdStatus, ordStatusRejected)
	rj.SetInt(TagCxlRejRespTo, 1) // response to a cancel request
	rj.Set(TagText, text)
	_ = sess.Send(rj)
}

// ---- market data ----

func (s *Server) snapshotMsg(reqID, fixSym string, snap *orderbook.Snapshot, depth int) *Message {
	w := NewMessage(MsgMarketDataSnap)
	w.Set(TagMDReqID, reqID)
	w.Set(TagSymbol, fixSym)
	bids := snap.Bids
	asks := snap.Asks
	if len(bids) > depth {
		bids = bids[:depth]
	}
	if len(asks) > depth {
		asks = asks[:depth]
	}
	w.SetInt(TagNoMDEntries, len(bids)+len(asks))
	for _, lv := range bids {
		w.Set(TagMDEntryType, "0") // bid
		w.Set(TagMDEntryPx, lv.Price.String())
		w.Set(TagMDEntrySize, lv.Volume.String())
	}
	for _, lv := range asks {
		w.Set(TagMDEntryType, "1") // offer
		w.Set(TagMDEntryPx, lv.Price.String())
		w.Set(TagMDEntrySize, lv.Volume.String())
	}
	return w
}

func (s *Server) mdReject(sess *Session, reqID, text string) {
	y := NewMessage(MsgMarketDataReject)
	y.Set(TagMDReqID, reqID)
	y.Set(TagText, text)
	_ = sess.Send(y)
}

// fanoutTrade sends an incremental refresh (X) of THIS trade to every session
// subscribed to the instrument's FIX symbol. It uses the trade data directly and
// must NOT call engine.Snapshot: this runs inside the order-book "trade" hook
// while the instrument book lock is held, so re-entering the book would deadlock.
func (s *Server) fanoutTrade(t *orderbook.Trade) {
	type target struct {
		sess  *Session
		reqID string
		sym   string
	}
	var targets []target
	s.mu.Lock()
	for sess, st := range s.sessions {
		for fixSym, reqID := range st.subs {
			if es, ok := s.resolve(fixSym); ok && es == t.Instrument {
				targets = append(targets, target{sess, reqID, fixSym})
			}
		}
	}
	s.mu.Unlock()
	for _, tg := range targets {
		x := NewMessage(MsgMarketDataInc)
		x.Set(TagMDReqID, tg.reqID)
		x.SetInt(TagNoMDEntries, 1)
		x.Set(TagMDUpdateAction, "0") // new
		x.Set(TagMDEntryType, "2")    // trade
		x.Set(TagSymbol, tg.sym)
		x.Set(TagMDEntryPx, t.Price.String())
		x.Set(TagMDEntrySize, t.Volume.String())
		_ = tg.sess.Send(x)
	}
}

func fixSideToEngine(side string) (orderbook.Side, bool) {
	switch side {
	case "1":
		return orderbook.SideBuy, true
	case "2":
		return orderbook.SideSell, true
	default:
		return "", false
	}
}
