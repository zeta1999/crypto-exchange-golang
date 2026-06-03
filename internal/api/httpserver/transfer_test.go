package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

type fakeEngine struct{}

func (fakeEngine) PlaceLimit(context.Context, *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	return nil, nil, nil
}
func (fakeEngine) PlaceMarket(context.Context, *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	return nil, nil, nil
}
func (fakeEngine) Snapshot(string) (*orderbook.Snapshot, error) { return nil, nil }

func TestTransferEndpoint(t *testing.T) {
	srv := New(fakeEngine{}, auth.NewTokenValidator("tok"), nil, nil)
	var gotFrom, gotTo, gotAsset string
	var gotAmt decimal.Decimal
	srv.SetTransfer(func(_ context.Context, from, to, asset string, amount decimal.Decimal) (string, error) {
		gotFrom, gotTo, gotAsset, gotAmt = from, to, asset, amount
		return "tx-9", nil
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/transfer", "application/json",
		strings.NewReader(`{"from":"binance","to":"coinbase","asset":"USDC","amount":"250.5","token":"tok"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["tx_ref"] != "tx-9" {
		t.Errorf("tx_ref = %q, want tx-9", out["tx_ref"])
	}
	if gotFrom != "binance" || gotTo != "coinbase" || gotAsset != "USDC" || gotAmt.Cmp(decimal.MustParse("250.5")) != 0 {
		t.Errorf("transfer fn got %s→%s %s %s", gotFrom, gotTo, gotAsset, gotAmt)
	}

	// Unauthorized token → 401.
	bad, err := http.Post(ts.URL+"/transfer", "application/json",
		strings.NewReader(`{"from":"a","to":"b","asset":"X","amount":"1","token":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token status = %d, want 401", bad.StatusCode)
	}
}

func TestTransferDisabled(t *testing.T) {
	srv := New(fakeEngine{}, auth.NewTokenValidator("tok"), nil, nil) // no SetTransfer
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/transfer", "application/json",
		strings.NewReader(`{"from":"a","to":"b","asset":"X","amount":"1","token":"tok"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("disabled transfer status = %d, want 501", resp.StatusCode)
	}
}
