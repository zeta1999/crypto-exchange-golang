package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func TestCoinbaseWithdrawEndpoint(t *testing.T) {
	var gotAsset, gotAddr string
	var gotAmt decimal.Decimal
	h := newHarness(t, WithWithdraw(func(_ context.Context, asset string, amount decimal.Decimal, dest string) (string, error) {
		gotAsset, gotAmt, gotAddr = asset, amount, dest
		return "tx-cb-1", nil
	}))

	body := `{"currency":"XLM","amount":"7.5","address":"GDEST"}`
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/withdraw", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["id"] != "tx-cb-1" {
		t.Errorf("withdraw id = %q, want tx-cb-1", out["id"])
	}
	if gotAsset != "XLM" || gotAddr != "GDEST" || gotAmt.Cmp(decimal.MustParse("7.5")) != 0 {
		t.Errorf("withdraw fn got asset=%q addr=%q amt=%s", gotAsset, gotAddr, gotAmt)
	}
}

func TestCoinbaseWithdrawDisabled(t *testing.T) {
	h := newHarness(t) // no WithWithdraw
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/withdraw", `{"currency":"XLM","amount":"1","address":"G"}`)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("withdraw should be rejected when not enabled")
	}
}
