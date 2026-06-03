package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func TestWithdrawEndpoint(t *testing.T) {
	var gotAsset, gotAddr string
	var gotAmt decimal.Decimal
	h := newHarness(t, WithWithdraw(func(_ context.Context, asset string, amount decimal.Decimal, dest string) (string, error) {
		gotAsset, gotAmt, gotAddr = asset, amount, dest
		return "txhash-123", nil
	}))

	p := url.Values{}
	p.Set("coin", "XLM")
	p.Set("address", "GDEST")
	p.Set("amount", "12.5")
	resp := h.signedDo(t, http.MethodPost, "/sapi/v1/capital/withdraw/apply", p)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var wr withdrawResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wr.ID != "txhash-123" {
		t.Errorf("withdraw id = %q, want txhash-123", wr.ID)
	}
	if gotAsset != "XLM" || gotAddr != "GDEST" || gotAmt.Cmp(decimal.MustParse("12.5")) != 0 {
		t.Errorf("withdraw fn got asset=%q addr=%q amt=%s", gotAsset, gotAddr, gotAmt)
	}
}

func TestWithdrawDisabled(t *testing.T) {
	h := newHarness(t) // no WithWithdraw
	p := url.Values{}
	p.Set("coin", "XLM")
	p.Set("address", "GDEST")
	p.Set("amount", "1")
	resp := h.signedDo(t, http.MethodPost, "/sapi/v1/capital/withdraw/apply", p)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("withdraw should be rejected when not enabled")
	}
}
