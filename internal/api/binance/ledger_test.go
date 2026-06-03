package binance

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func (h *testHarness) account(t *testing.T) map[string]Balance {
	t.Helper()
	resp := h.signedDo(t, http.MethodGet, "/api/v3/account", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/account status = %d", resp.StatusCode)
	}
	var acct accountResponse
	if err := json.NewDecoder(resp.Body).Decode(&acct); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	out := map[string]Balance{}
	for _, b := range acct.Balances {
		out[b.Asset] = b
	}
	return out
}

func placeLimit(t *testing.T, h *testHarness, side, qty, price string) *http.Response {
	t.Helper()
	p := url.Values{}
	p.Set("symbol", "BTCUSDT")
	p.Set("side", side)
	p.Set("type", "LIMIT")
	p.Set("timeInForce", "GTC")
	p.Set("quantity", qty)
	p.Set("price", price)
	return h.signedDo(t, http.MethodPost, "/api/v3/order", p)
}

// TestAccountLedgerSettlesFill: a LIMIT buy that crosses a resting ask locks
// quote on placement and, on fill, debits USD + credits BTC in /account.
func TestAccountLedgerSettlesFill(t *testing.T) {
	led := account.NewLedger(map[string]decimal.Decimal{"USD": decimal.FromInt(1_000_000)})
	h := newHarness(t, WithLedger(led))

	// Resting counterparty ask (non-edge id → ledger ignores it) at 100, vol 5.
	mustAdd(t, h.book, "synth-ask", orderbook.SideSell, "100", "5")

	resp := placeLimit(t, h, "BUY", "2", "100")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	bals := h.account(t)
	if got := bals["USD"].Free; got != "999800.00000000" {
		t.Errorf("USD free = %s, want 999800 (1,000,000 - 2*100)", got)
	}
	if got := bals["USD"].Locked; got != "0.00000000" {
		t.Errorf("USD locked = %s, want 0 (fully settled)", got)
	}
	if got := bals["BTC"].Free; got != "2.00000000" {
		t.Errorf("BTC free = %s, want 2", got)
	}
}

// TestAccountLedgerLocksResting: a LIMIT buy that does NOT cross rests and keeps
// its quote locked (free reduced, locked increased).
func TestAccountLedgerLocksResting(t *testing.T) {
	led := account.NewLedger(map[string]decimal.Decimal{"USD": decimal.FromInt(1000)})
	h := newHarness(t, WithLedger(led))

	resp := placeLimit(t, h, "BUY", "3", "100") // 300 reserved, nothing to fill
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("place status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	bals := h.account(t)
	if bals["USD"].Free != "700.00000000" || bals["USD"].Locked != "300.00000000" {
		t.Errorf("USD = free %s / locked %s, want 700 / 300", bals["USD"].Free, bals["USD"].Locked)
	}
}

// TestAccountLedgerInsufficient: an order exceeding free balance is rejected.
func TestAccountLedgerInsufficient(t *testing.T) {
	led := account.NewLedger(map[string]decimal.Decimal{"USD": decimal.FromInt(50)})
	h := newHarness(t, WithLedger(led))

	resp := placeLimit(t, h, "BUY", "2", "100") // needs 200, have 50
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("order exceeding balance should be rejected")
	}
	var e struct {
		Code int `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Code != -2010 {
		t.Errorf("error code = %d, want -2010 (insufficient balance)", e.Code)
	}
	// Balance untouched.
	if h.account(t)["USD"].Free != "50.00000000" {
		t.Error("rejected order must not move balance")
	}
}
