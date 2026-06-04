package fix

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

type noopMargin struct{}

func (noopMargin) Validate(context.Context, *orderbook.Order) error { return nil }

// fixHarness wires a real engine + book to a FIX acceptor over an in-memory pipe,
// with a fixed clock. A client goroutine collects inbound frames.
type fixHarness struct {
	eng  *engine.Engine
	book *orderbook.OrderBook
	cli  *fixClient
}

func newFixHarness(t *testing.T) *fixHarness {
	t.Helper()
	book := orderbook.New([]string{"BTC-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	clock := func() time.Time { return time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC) }
	resolve := func(sym string) (string, bool) {
		if sym == "BTCUSDT" {
			return "BTC-USD", true
		}
		return "", false
	}
	srv := NewServer(eng, resolve, "MIRAGE", clock)
	srv.AttachHooks(book)

	cConn, sConn := net.Pipe()
	t.Cleanup(func() { cConn.Close(); sConn.Close() })
	go func() { _ = srv.Accept(sConn) }()

	cli := &fixClient{conn: cConn, r: bufio.NewReader(cConn), seq: 1, in: make(chan *Message, 64), t: t}
	go cli.readLoop()
	return &fixHarness{eng: eng, book: book, cli: cli}
}

type fixClient struct {
	conn net.Conn
	r    *bufio.Reader
	seq  int
	in   chan *Message
	t    *testing.T
}

func (c *fixClient) readLoop() {
	for {
		m, err := ReadMessage(c.r)
		if err != nil {
			close(c.in)
			return
		}
		c.in <- m
	}
}

func (c *fixClient) send(m *Message) {
	m.Set(TagSenderCompID, "VIVALDI")
	m.Set(TagTargetCompID, "MIRAGE")
	m.SetInt(TagMsgSeqNum, c.seq)
	c.seq++
	m.Set(TagSendingTime, "20260604-08:00:00.000")
	if _, err := c.conn.Write(m.Encode()); err != nil {
		c.t.Fatalf("client write: %v", err)
	}
}

func (c *fixClient) expect(msgType string) *Message {
	c.t.Helper()
	select {
	case m, ok := <-c.in:
		if !ok {
			c.t.Fatalf("connection closed waiting for %q", msgType)
		}
		if m.MsgType() != msgType {
			c.t.Fatalf("want MsgType %q, got %q: %+v", msgType, m.MsgType(), m.Fields)
		}
		return m
	case <-time.After(2 * time.Second):
		c.t.Fatalf("timeout waiting for %q", msgType)
	}
	return nil
}

// drain collects every frame that arrives within d.
func (c *fixClient) drain(d time.Duration) []*Message {
	var out []*Message
	timer := time.After(d)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-timer:
			return out
		}
	}
}

func (c *fixClient) logon() {
	m := NewMessage(MsgLogon)
	m.SetInt(TagEncryptMethod, 0)
	m.SetInt(TagHeartBtInt, 30)
	c.send(m)
	c.expect(MsgLogon) // acceptor's Logon ack
}

func newOrder(clOrdID string, side string, qty, ordType, price string) *Message {
	m := NewMessage(MsgNewOrderSingle)
	m.Set(TagClOrdID, clOrdID)
	m.Set(TagSymbol, "BTCUSDT")
	m.Set(TagSide, side)
	m.Set(TagOrderQty, qty)
	m.Set(TagOrdType, ordType)
	if price != "" {
		m.Set(TagPrice, price)
	}
	return m
}

func TestFIX_Logon(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
}

func TestFIX_PlaceRestsAndAcksNew(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(newOrder("ord-1", "1", "1.0", "2", "50000"))
	er := h.cli.expect(MsgExecutionReport)
	if v, _ := er.Get(TagExecType); v != execTypeNew {
		t.Errorf("ExecType=%q want New", v)
	}
	if v, _ := er.Get(TagOrdStatus); v != ordStatusNew {
		t.Errorf("OrdStatus=%q want New", v)
	}
	if v, _ := er.Get(TagClOrdID); v != "ord-1" {
		t.Errorf("ClOrdID=%q", v)
	}
	if v, _ := er.Get(TagLeavesQty); v != decimal.MustParse("1.0").String() {
		t.Errorf("LeavesQty=%q", v)
	}
}

func TestFIX_FillEmitsTradeReport(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(newOrder("buy-1", "1", "2.0", "2", "50000"))
	h.cli.expect(MsgExecutionReport) // New

	// A counterparty SELL crosses the resting FIX buy (placed straight on the
	// engine, as if another participant). The book hook routes the fill to FIX.
	_, _, err := h.eng.PlaceLimit(context.Background(), &orderbook.Order{
		ID: "cp:1", Instrument: "BTC-USD", Price: decimal.MustParse("50000"),
		Volume: decimal.MustParse("2.0"), Side: orderbook.SideSell,
	})
	if err != nil {
		t.Fatal(err)
	}
	er := h.cli.expect(MsgExecutionReport) // Trade
	if v, _ := er.Get(TagExecType); v != execTypeTrade {
		t.Errorf("ExecType=%q want Trade(F)", v)
	}
	if v, _ := er.Get(TagOrdStatus); v != ordStatusFilled {
		t.Errorf("OrdStatus=%q want Filled", v)
	}
	if v, _ := er.Get(TagLastQty); v != decimal.MustParse("2.0").String() {
		t.Errorf("LastQty=%q", v)
	}
	if v, _ := er.Get(TagCumQty); v != decimal.MustParse("2.0").String() {
		t.Errorf("CumQty=%q", v)
	}
}

func TestFIX_DuplicateClOrdIDRejected(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(newOrder("dup-1", "1", "1.0", "2", "49000"))
	h.cli.expect(MsgExecutionReport) // New
	// Same ClOrdID again → Rejected, never a 2nd resting order.
	h.cli.send(newOrder("dup-1", "1", "1.0", "2", "49000"))
	er := h.cli.expect(MsgExecutionReport)
	if v, _ := er.Get(TagExecType); v != execTypeRejected {
		t.Errorf("ExecType=%q want Rejected", v)
	}
	// Only ONE order rests at 49000.
	snap, _ := h.eng.Snapshot("BTC-USD")
	total := decimal.Zero
	for _, b := range snap.Bids {
		if b.Price.Cmp(decimal.MustParse("49000")) == 0 {
			total = total.Add(b.Volume)
		}
	}
	if total.Cmp(decimal.MustParse("1.0")) != 0 {
		t.Errorf("duplicate created extra resting size: %s", total.String())
	}
}

func TestFIX_CancelCancels(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(newOrder("c-1", "1", "1.0", "2", "48000"))
	h.cli.expect(MsgExecutionReport) // New

	cxl := NewMessage(MsgOrderCancelReq)
	cxl.Set(TagClOrdID, "cxl-1")
	cxl.Set(TagOrigClOrdID, "c-1")
	cxl.Set(TagSymbol, "BTCUSDT")
	cxl.Set(TagSide, "1")
	h.cli.send(cxl)
	er := h.cli.expect(MsgExecutionReport)
	if v, _ := er.Get(TagExecType); v != execTypeCanceled {
		t.Errorf("ExecType=%q want Canceled", v)
	}
	if v, _ := er.Get(TagOrigClOrdID); v != "c-1" {
		t.Errorf("OrigClOrdID=%q", v)
	}

	// Cancelling an unknown order → OrderCancelReject (9).
	cxl2 := NewMessage(MsgOrderCancelReq)
	cxl2.Set(TagClOrdID, "cxl-2")
	cxl2.Set(TagOrigClOrdID, "nope")
	cxl2.Set(TagSymbol, "BTCUSDT")
	cxl2.Set(TagSide, "1")
	h.cli.send(cxl2)
	h.cli.expect(MsgOrderCancelReject)
}

func TestFIX_MarketDataSnapshot(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	// Seed both sides of the book.
	h.cli.send(newOrder("b-1", "1", "1.0", "2", "49900"))
	h.cli.expect(MsgExecutionReport)
	h.cli.send(newOrder("a-1", "2", "1.0", "2", "50100"))
	h.cli.expect(MsgExecutionReport)

	v := NewMessage(MsgMarketDataReq)
	v.Set(TagMDReqID, "md-1")
	v.Set(TagSubReqType, "0") // snapshot
	v.SetInt(TagMarketDepth, 10)
	v.SetInt(TagNoMDEntryTypes, 2)
	v.Set(TagMDEntryType, "0")
	v.Set(TagMDEntryType, "1")
	v.SetInt(TagNoRelatedSym, 1)
	v.Set(TagSymbol, "BTCUSDT")
	h.cli.send(v)

	w := h.cli.expect(MsgMarketDataSnap)
	if id, _ := w.Get(TagMDReqID); id != "md-1" {
		t.Errorf("MDReqID=%q", id)
	}
	n, _ := w.GetInt(TagNoMDEntries)
	if n != 2 {
		t.Errorf("NoMDEntries=%d want 2", n)
	}
	// One bid (269=0) and one offer (269=1) present.
	var bid, ask bool
	for _, f := range w.Fields {
		if f.Tag == TagMDEntryType && f.Value == "0" {
			bid = true
		}
		if f.Tag == TagMDEntryType && f.Value == "1" {
			ask = true
		}
	}
	if !bid || !ask {
		t.Errorf("snapshot missing a side: bid=%v ask=%v", bid, ask)
	}
}

func TestFIX_MissingRequiredTagRejected(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	// NewOrderSingle without OrderQty (38) → session-level Reject (3).
	m := NewMessage(MsgNewOrderSingle)
	m.Set(TagClOrdID, "bad-1")
	m.Set(TagSymbol, "BTCUSDT")
	m.Set(TagSide, "1")
	m.Set(TagOrdType, "2")
	m.Set(TagPrice, "50000")
	h.cli.send(m)
	rj := h.cli.expect(MsgReject)
	if tag, _ := rj.GetInt(TagRefTagID); tag != TagOrderQty {
		t.Errorf("RefTagID=%d want %d", tag, TagOrderQty)
	}
}

// TestFIX_CancelRacesFill drives the exact production race the brutal review
// flagged: a resting FIX order is filled by a counterparty on ONE goroutine (the
// book "trade" hook -> onFill writes rec state under s.mu) while the owning
// session processes a Cancel for the SAME order on its read goroutine. Run with
// -race. The order must end in exactly one terminal state, never both.
func TestFIX_CancelRacesFill(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(newOrder("race-1", "1", "2.0", "2", "50000"))
	h.cli.expect(MsgExecutionReport) // New

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = h.eng.PlaceLimit(context.Background(), &orderbook.Order{
			ID: "cp-race:1", Instrument: "BTC-USD", Price: decimal.MustParse("50000"),
			Volume: decimal.MustParse("2.0"), Side: orderbook.SideSell,
		})
	}()
	cxl := NewMessage(MsgOrderCancelReq)
	cxl.Set(TagClOrdID, "cxl-race")
	cxl.Set(TagOrigClOrdID, "race-1")
	cxl.Set(TagSymbol, "BTCUSDT")
	cxl.Set(TagSide, "1")
	h.cli.send(cxl)
	wg.Wait()

	var filled, canceled bool
	for _, m := range h.cli.drain(300 * time.Millisecond) {
		if m.MsgType() != MsgExecutionReport {
			continue
		}
		et, _ := m.Get(TagExecType)
		st, _ := m.Get(TagOrdStatus)
		if et == execTypeTrade && st == ordStatusFilled {
			filled = true
		}
		if et == execTypeCanceled {
			canceled = true
		}
	}
	if filled && canceled {
		t.Error("order reported BOTH Filled and Canceled — inconsistent terminal state")
	}
}

func TestFIX_UnknownSymbolRejected(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	m := NewMessage(MsgNewOrderSingle)
	m.Set(TagClOrdID, "u-1")
	m.Set(TagSymbol, "DOGEUSDT") // not resolvable
	m.Set(TagSide, "1")
	m.Set(TagOrderQty, "1.0")
	m.Set(TagOrdType, "2")
	m.Set(TagPrice, "50000")
	h.cli.send(m)
	er := h.cli.expect(MsgExecutionReport)
	if v, _ := er.Get(TagExecType); v != execTypeRejected {
		t.Errorf("ExecType=%q want Rejected for unknown symbol", v)
	}
}
