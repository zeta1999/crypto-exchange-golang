package transfer

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/custody"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

type sendCall struct{ asset, dest, amount, ref string }

// fakeBackend records sends and replays queued deposits, honouring the cursor.
type fakeBackend struct {
	sends    []sendCall
	deposits map[string][]custody.Payment
	sendErr  error
}

func (f *fakeBackend) Send(_ context.Context, _ []byte, asset, dest, amount string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	ref := "tx-" + strconv.Itoa(len(f.sends))
	f.sends = append(f.sends, sendCall{asset, dest, amount, ref})
	return ref, nil
}

func (f *fakeBackend) Received(_ context.Context, address, cursor string) ([]custody.Payment, error) {
	var out []custody.Payment
	for _, p := range f.deposits[address] {
		if p.Cursor > cursor { // lexical; test uses "1","2",…
			out = append(out, p)
		}
	}
	return out, nil
}

func led(free string) *account.Ledger {
	return account.NewLedger(map[string]decimal.Decimal{"XLM": decimal.MustParse(free)})
}

func TestHubWithdrawDebitsAndSends(t *testing.T) {
	be := &fakeBackend{deposits: map[string][]custody.Payment{}}
	h := NewHub(be, time.Second)
	lb, lc := led("1000"), led("0")
	h.AddVenue("binance", lb, []byte("s-b"), "addrB")
	h.AddVenue("coinbase", lc, []byte("s-c"), "addrC")

	ref, err := h.Withdraw(context.Background(), "binance", "XLM", decimal.MustParse("100"), "addrC")
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if ref == "" || len(be.sends) != 1 || be.sends[0].dest != "addrC" || be.sends[0].amount != "100.0000000" {
		t.Fatalf("send not recorded correctly: %+v", be.sends)
	}
	if lb.Get("XLM").Free.Cmp(decimal.MustParse("900")) != 0 {
		t.Errorf("source free = %s, want 900 (debited)", lb.Get("XLM").Free)
	}
	if h.Inflight()[0].Status != "pending" {
		t.Errorf("transfer should be pending until deposit lands")
	}

	// Funds arrive at the destination → poll credits + settles.
	be.deposits["addrC"] = []custody.Payment{{TxRef: ref, Asset: "XLM", Amount: "100", Cursor: "1"}}
	h.poll(context.Background())
	if lc.Get("XLM").Free.Cmp(decimal.MustParse("100")) != 0 {
		t.Errorf("dest free = %s, want 100 (credited)", lc.Get("XLM").Free)
	}
	if h.Inflight()[0].Status != "settled" {
		t.Errorf("transfer should be settled after deposit")
	}
	// Idempotent: a second poll does not double-credit.
	h.poll(context.Background())
	if lc.Get("XLM").Free.Cmp(decimal.MustParse("100")) != 0 {
		t.Errorf("dest free = %s after re-poll, want 100 (no double credit)", lc.Get("XLM").Free)
	}
}

// TestHubWithdrawQuantizes: an amount with more than the chain's precision is
// quantized once, so the ledger debit and the on-chain send move the SAME
// amount (no value burned between debit and send).
func TestHubWithdrawQuantizes(t *testing.T) {
	be := &fakeBackend{deposits: map[string][]custody.Payment{}}
	h := NewHub(be, time.Second)
	lb := led("1000")
	h.AddVenue("binance", lb, []byte("s"), "addrB")
	if _, err := h.Withdraw(context.Background(), "binance", "XLM", decimal.MustParse("1.00000009"), "addrC"); err != nil {
		t.Fatal(err)
	}
	if be.sends[0].amount != "1.0000000" {
		t.Errorf("send amount = %s, want 1.0000000 (7dp)", be.sends[0].amount)
	}
	// 1000 - 1.0000000 = 999 (debit equals the sent amount, not the 8dp input).
	if lb.Get("XLM").Free.Cmp(decimal.MustParse("999")) != 0 {
		t.Errorf("debited to %s, want 999 (debit==send, no leak)", lb.Get("XLM").Free)
	}
}

func TestHubWithdrawInsufficient(t *testing.T) {
	be := &fakeBackend{deposits: map[string][]custody.Payment{}}
	h := NewHub(be, time.Second)
	h.AddVenue("binance", led("50"), []byte("s"), "addrB")
	if _, err := h.Withdraw(context.Background(), "binance", "XLM", decimal.MustParse("100"), "addrC"); err == nil {
		t.Fatal("withdraw over balance should fail")
	}
	if len(be.sends) != 0 {
		t.Error("no send should happen on insufficient balance")
	}
}

func TestHubWithdrawSendFailureRefunds(t *testing.T) {
	be := &fakeBackend{deposits: map[string][]custody.Payment{}, sendErr: errors.New("horizon down")}
	h := NewHub(be, time.Second)
	lb := led("1000")
	h.AddVenue("binance", lb, []byte("s"), "addrB")
	if _, err := h.Withdraw(context.Background(), "binance", "XLM", decimal.MustParse("100"), "addrC"); err == nil {
		t.Fatal("withdraw should fail when send fails")
	}
	if lb.Get("XLM").Free.Cmp(decimal.MustParse("1000")) != 0 {
		t.Errorf("balance = %s, want 1000 (refunded after send failure)", lb.Get("XLM").Free)
	}
}
