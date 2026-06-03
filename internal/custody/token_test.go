package custody

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCircleDripNoKeyIsManual: with no CIRCLE_API_KEY, USDC funding falls back
// to the manual (web) faucet rather than calling the API.
func TestCircleDripNoKeyIsManual(t *testing.T) {
	t.Setenv("CIRCLE_API_KEY", "")
	if _, err := circleDrip(context.Background(), defaultHTTPClient(), "XLM-TESTNET", "GADDR"); err != ErrManualFaucet {
		t.Fatalf("circleDrip without key = %v, want ErrManualFaucet", err)
	}

	s := NewStellar()
	if _, err := s.Tap(context.Background(), "GADDR", "USDC", 0); err != ErrManualFaucet {
		t.Errorf("xlm USDC tap without key = %v, want ErrManualFaucet", err)
	}
	if url, ok := s.ManualURL("USDC"); !ok || url == "" {
		t.Errorf("xlm USDC ManualURL = %q,%v, want a URL", url, ok)
	}
	sol := NewSolana()
	if url, ok := sol.ManualURL("USDC"); !ok || url == "" {
		t.Errorf("sol USDC ManualURL = %q,%v, want a URL", url, ok)
	}
}

// TestStellarPrepareAssetValidation covers the non-network validation paths.
func TestStellarPrepareAssetValidation(t *testing.T) {
	s := NewStellar()
	if _, err := s.PrepareAsset(context.Background(), make([]byte, 32), "FOO"); err != ErrUnsupportedAsset {
		t.Errorf("unsupported asset err = %v, want ErrUnsupportedAsset", err)
	}
	if _, err := s.PrepareAsset(context.Background(), []byte("short"), "USDC"); err == nil {
		t.Error("bad seed length should error before any network call")
	}
}

// TestSolanaSPLBalanceParsing drives Balances against a fake RPC returning a
// getTokenAccountsByOwner jsonParsed result, asserting USDC is summed.
func TestSolanaSPLBalanceParsing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := readAll(r)
		_ = json.Unmarshal(body, &req)
		var result string
		switch req.Method {
		case "getBalance":
			result = `{"value":2000000000}` // 2 SOL
		case "getTokenAccountsByOwner":
			result = `{"value":[{"account":{"data":{"parsed":{"info":{"tokenAmount":{"amount":"1500000","decimals":6,"uiAmountString":"1.5"}}}}}}]}`
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}))
	defer ts.Close()

	s := NewSolana()
	s.rpc = ts.URL // in-package override
	bals, err := s.Balances(context.Background(), "SomeOwner")
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	got := map[string]string{}
	for _, b := range bals {
		got[b.Asset] = b.Amount
	}
	if got["SOL"] != "2" {
		t.Errorf("SOL = %q, want 2", got["SOL"])
	}
	if got["USDC"] != "1.5" {
		t.Errorf("USDC = %q, want 1.5", got["USDC"])
	}
}

// TestSolanaSPLBalanceAbsent: no token account → USDC omitted, not shown as 0.
func TestSolanaSPLBalanceAbsent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := readAll(r)
		_ = json.Unmarshal(body, &req)
		result := `{"value":0}`
		if req.Method == "getTokenAccountsByOwner" {
			result = `{"value":[]}`
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}))
	defer ts.Close()
	s := NewSolana()
	s.rpc = ts.URL
	bals, err := s.Balances(context.Background(), "Owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bals {
		if b.Asset == "USDC" {
			t.Errorf("USDC should be omitted when no token account, got %v", b)
		}
	}
}
