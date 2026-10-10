package fix

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// TestManualSmoke drives a REAL server (127.0.0.1:18199) built with quotes
// enabled, using the same fixClient the race-tests use. It skips in CI (no
// server), and is the end-to-end check that saga pricing + the z→S→D quote
// flow work live against a real book. Run:
//
//	EXCHANGE_CONFIG=<quotes-enabled-config> go run ./cmd/exchange &
//	go test ./internal/api/fix/ -run TestManualSmoke -v
func TestManualSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("manual")
	}
	conn, err := net.Dial("tcp", "127.0.0.1:18199")
	if err != nil {
		t.Skipf("no live server on :18199 (run one with quotes enabled): %v", err)
	}
	defer conn.Close()
	cli := &fixClient{conn: conn, r: bufio.NewReader(conn), seq: 1, in: make(chan *Message, 64), t: t}
	go cli.readLoop()
	cli.logon()

	// 1) QuoteRequest (z) for the exotic → expect a Quote (S) from the saga pricer.
	qreq := NewMessage(MsgQuoteRequest)
	qreq.Set(TagQuoteReqID, "Q1")
	qreq.Set(TagSymbol, "BTC-EXOT-C")
	qreq.Set(TagSide, "1")
	qreq.Set(TagOrderQty, "100")
	cli.send(qreq)
	m := cli.expect(MsgQuote) // expect() times out at 2s; saga is ~0.2s
	qid, _ := m.Get(TagQuoteID)
	bid, _ := m.Get(TagBidPx)
	offer, _ := m.Get(TagOfferPx)
	t.Logf("QuoteID=%s BidPx=%s OfferPx=%s", qid, bid, offer)
	if bid == "" || offer == "" {
		t.Fatalf("quote missing prices: %+v", m.Fields)
	}

	// 2) Confirm: NewOrderSingle QuotedOrderFlag=Y + OrderID=QuoteID → NEW.
	d := NewMessage(MsgNewOrderSingle)
	d.Set(TagClOrdID, "CL1")
	d.Set(TagSymbol, "BTC-EXOT-C")
	d.Set(TagSide, "1")
	d.Set(TagOrderQty, "100")
	d.Set(TagOrdType, "2")
	d.Set(TagQuotedOrderFlag, "Y")
	d.Set(TagOrderID, qid)
	cli.send(d)
	n := cli.expect(MsgExecutionReport)
	if et, _ := n.Get(TagExecType); et != execTypeNew {
		t.Fatalf("want NEW, got ExecType=%s %+v", et, n.Fields)
	}
	t.Logf("NEW at quoted price: CumQty=%s", mustGet(n, TagCumQty))

	// 3) Reuse the same quote → expect a Rejected ExecutionReport.
	d2 := NewMessage(MsgNewOrderSingle)
	d2.Set(TagClOrdID, "CL2")
	d2.Set(TagSymbol, "BTC-EXOT-C")
	d2.Set(TagSide, "1")
	d2.Set(TagOrderQty, "100")
	d2.Set(TagOrdType, "2")
	d2.Set(TagQuotedOrderFlag, "Y")
	d2.Set(TagOrderID, qid)
	cli.send(d2)
	gotReject := false
	deadline := time.After(5 * time.Second)
	for !gotReject {
		select {
		case m := <-cli.in:
			if m.MsgType() != MsgExecutionReport {
				continue
			}
			if et, _ := m.Get(TagExecType); et == execTypeRejected {
				gotReject = true
			}
		case <-deadline:
			t.Fatal("no Rejected report for the reused quote within 5s")
		}
	}
	t.Log("manual smoke: z→S (saga-priced) → D→NEW → reuse→Rejected: OK")
}

func mustGet(m *Message, tag int) string {
	v, _ := m.Get(tag)
	return v
}
