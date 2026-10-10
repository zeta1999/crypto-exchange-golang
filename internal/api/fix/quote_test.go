package fix

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// mockQuoter is a deterministic Quoter: fixed bid/offer per instrument, counts
// how many times it was consulted, and marks configured symbols exotic.
type mockQuoter struct {
	mu        sync.Mutex
	exotics   map[string]bool
	bid       decimal.Decimal
	offer     decimal.Decimal
	calls     int
	shouldErr bool
}

func (q *mockQuoter) Quote(ctx context.Context, engineSymbol string, qty decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	q.mu.Lock()
	q.calls++
	q.mu.Unlock()
	if q.shouldErr {
		return decimal.Zero, decimal.Zero, fmt.Errorf("simulated pricing failure")
	}
	return q.bid, q.offer, nil
}

func (q *mockQuoter) IsExotic(engineSymbol string) bool { return q.exotics[engineSymbol] }

func (q *mockQuoter) Calls() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

const quoteTestExotic = "EXOTIC"     // engine symbol
const quoteTestFixSym = "BTC-EXOT-C" // FIX symbol for it

type quoteHarness struct {
	cli    *fixClient
	book   *orderbook.OrderBook
	quoter *mockQuoter
}

func newQuoteHarness(t *testing.T) *quoteHarness {
	t.Helper()
	book := orderbook.New([]string{"BTC-USD", quoteTestExotic})
	eng := engine.New(book, noopMargin{}, nil)
	clock := func() time.Time { return time.Date(2026, 6, 4, 8, 0, 0, 0, time.UTC) }
	resolve := func(sym string) (string, bool) {
		switch sym {
		case "BTCUSDT":
			return "BTC-USD", true
		case quoteTestFixSym:
			return quoteTestExotic, true
		}
		return "", false
	}
	srv := NewServer(eng, resolve, "MIRAGE", clock)
	srv.AttachHooks(book)
	quoter := &mockQuoter{
		exotics: map[string]bool{quoteTestExotic: true},
		bid:     decimal.MustParse("99.5"),
		offer:   decimal.MustParse("101.5"),
	}
	srv.SetQuoter(quoter)

	cConn, sConn := net.Pipe()
	t.Cleanup(func() { cConn.Close(); sConn.Close() })
	go func() { _ = srv.Accept(sConn) }()
	cli := &fixClient{conn: cConn, r: bufio.NewReader(cConn), seq: 1, in: make(chan *Message, 64), t: t}
	go cli.readLoop()
	return &quoteHarness{cli: cli, book: book, quoter: quoter}
}

func (qh *quoteHarness) logon() {
	qh.cli.logon()
}

// requestQuote sends a 35=z and returns the Quote (S).
func (qh *quoteHarness) requestQuote(reqID, side, qty string) *Message {
	qh.cli.send(quoteRequest(reqID, quoteTestFixSym, side, qty))
	m := qh.cli.expect(MsgQuote)
	if m.MsgType() != MsgQuote { // expect() already checks; keep for clarity
		qh.cli.t.Fatalf("want Quote, got %q", m.MsgType())
	}
	return m
}

func quoteRequest(reqID, symbol, side, qty string) *Message {
	m := NewMessage(MsgQuoteRequest)
	m.Set(TagQuoteReqID, reqID)
	m.Set(TagSymbol, symbol)
	m.Set(TagSide, side)
	m.Set(TagOrderQty, qty)
	return m
}

// confirmQuote sends a NewOrderSingle carrying QuotedOrderFlag=Y + OrderID=quoteID.
func confirmQuote(clOrdID, quoteID, side, qty string) *Message {
	d := NewMessage(MsgNewOrderSingle)
	d.Set(TagClOrdID, clOrdID)
	d.Set(TagSymbol, quoteTestFixSym)
	d.Set(TagSide, side)
	d.Set(TagOrderQty, qty)
	d.Set(TagOrdType, "2")
	d.Set(TagQuotedOrderFlag, "Y")
	d.Set(TagOrderID, quoteID)
	return d
}

// TestFIX_QuoteRequest_RespondsS: z → S with executable bid/offer.
func TestFIX_QuoteRequest_RespondsS(t *testing.T) {
	qh := newQuoteHarness(t)
	qh.logon()
	m := qh.requestQuote("Q1", "1", "10")

	if id, _ := m.Get(TagQuoteID); id == "" {
		t.Fatalf("quote missing QuoteID: %+v", m.Fields)
	}
	if bid, _ := m.Get(TagBidPx); bid != decimal.MustParse("99.5").String() {
		t.Errorf("BidPx = %q, want %q", bid, decimal.MustParse("99.5").String())
	}
	if offer, _ := m.Get(TagOfferPx); offer != decimal.MustParse("101.5").String() {
		t.Errorf("OfferPx = %q, want %q", offer, decimal.MustParse("101.5").String())
	}
	if rid, _ := m.Get(TagQuoteReqID); rid != "Q1" {
		t.Errorf("QuoteReqID = %q, want Q1", rid)
	}
	if qh.quoter.Calls() != 1 {
		t.Errorf("quoter consulted %d times, want 1", qh.quoter.Calls())
	}
}

// TestFIX_QuotedOrder_RestsAtQuotedPrice: a buy confirmation fills nothing on
// an empty book but RESTS at the quoted offer; a later crossing quote buy
// (lower offer) would trade it — here we just assert the resting price is the
// QUOTED one, not whatever the client sent.
func TestFIX_QuotedOrder_RestsAtQuotedPrice(t *testing.T) {
	qh := newQuoteHarness(t)
	qh.logon()
	q := qh.requestQuote("Q1", "1", "10")
	quoteID, _ := q.Get(TagQuoteID)

	qh.cli.send(confirmQuote("CL1", quoteID, "1", "10"))
	qh.cli.expect(MsgExecutionReport) // NEW

	snap, err := qh.book.Snapshot(quoteTestExotic)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.BestBid != decimal.MustParse("101.5") {
		t.Errorf("quoted buy should rest at offer 101.5; bestBid=%s", snap.BestBid)
	}
}

// TestFIX_QuotedOrder_DuplicateRejected: reusing a quote a second time → reject.
func TestFIX_QuotedOrder_DuplicateRejected(t *testing.T) {
	qh := newQuoteHarness(t)
	qh.logon()
	q := qh.requestQuote("Q1", "1", "10")
	quoteID, _ := q.Get(TagQuoteID)

	qh.cli.send(confirmQuote("CL1", quoteID, "1", "10"))
	qh.cli.expect(MsgExecutionReport) // NEW

	qh.cli.send(confirmQuote("CL2", quoteID, "1", "10"))
	foundReject := false
	for _, m := range qh.cli.drain(300 * time.Millisecond) {
		if m.MsgType() == MsgExecutionReport {
			if et, _ := m.Get(TagExecType); et == execTypeRejected {
				foundReject = true
			}
		}
	}
	if !foundReject {
		t.Error("expected a Rejected ExecutionReport for the reused quote")
	}
}

// TestFIX_PlainExoticOrder_AutoQuotes: a plain D on an exotic (no quote flag)
// is answered with a Quote, not traded.
func TestFIX_PlainExoticOrder_AutoQuotes(t *testing.T) {
	qh := newQuoteHarness(t)
	qh.logon()
	d := NewMessage(MsgNewOrderSingle)
	d.Set(TagClOrdID, "CL1")
	d.Set(TagSymbol, quoteTestFixSym)
	d.Set(TagSide, "2")
	d.Set(TagOrderQty, "5")
	d.Set(TagOrdType, "2")
	d.Set(TagPrice, "1")
	qh.cli.send(d)

	m := qh.cli.expect(MsgQuote)
	if _, ok := m.Get(TagOfferPx); !ok {
		t.Errorf("auto-quote missing OfferPx: %+v", m.Fields)
	}
	// And nothing got traded/rested.
	snap, _ := qh.book.Snapshot(quoteTestExotic)
	if snap != nil && (snap.BestBid.Sign() > 0 || snap.BestAsk.Sign() > 0) {
		t.Errorf("plain exotic order should not rest; snapshot=%+v", snap)
	}
}

// TestFIX_QuoteRequest_QuoterFailure: a pricing failure → reject, no Quote.
func TestFIX_QuoteRequest_QuoterFailure(t *testing.T) {
	qh := newQuoteHarness(t)
	qh.quoter.shouldErr = true
	qh.logon()
	qh.cli.send(quoteRequest("Q1", quoteTestFixSym, "1", "10"))
	foundReject, sawQuote := false, false
	for _, m := range qh.cli.drain(300 * time.Millisecond) {
		if m.MsgType() == MsgQuote {
			sawQuote = true
		}
		if m.MsgType() == MsgExecutionReport {
			if et, _ := m.Get(TagExecType); et == execTypeRejected {
				foundReject = true
			}
		}
	}
	if sawQuote {
		t.Error("unexpected Quote despite a quoter failure")
	}
	if !foundReject {
		t.Error("expected a reject after a quoter failure")
	}
}

// TestFIX_QuoteRequest_NoQuoter: without a quoter, z → business reject.
func TestFIX_QuoteRequest_NoQuoter(t *testing.T) {
	h := newFixHarness(t)
	h.cli.logon()
	h.cli.send(quoteRequest("Q1", "BTCUSDT", "1", "10"))
	sawReject := false
	for _, m := range h.cli.drain(300 * time.Millisecond) {
		if m.MsgType() == MsgQuote {
			t.Fatalf("unexpected Quote without a quoter: %+v", m.Fields)
		}
		if m.MsgType() == MsgBusinessReject {
			sawReject = true
		}
	}
	if !sawReject {
		t.Fatal("expected a BusinessReject for a QuoteRequest with no quoter")
	}
}
