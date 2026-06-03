package custody

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEVMReceived_USDC drives EVM.Received against a fake eth_getLogs that
// returns one USDC ERC20 Transfer credited to the watched address; the watcher
// must emit a USDC payment with the decoded amount and a next-block cursor.
func TestEVMReceived_USDC(t *testing.T) {
	usdc := sepoliaTokens[0] // {USDC, 0x1c7D…, 6}
	// 2.5 USDC at 6 decimals = 2_500_000 -> 0x2625a0, left-padded to 32 bytes.
	const dataHex = "0x00000000000000000000000000000000000000000000000000000000002625a0"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "eth_getLogs" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
			return
		}
		logs := []any{map[string]any{
			"address":         usdc.Contract,
			"data":            dataHex,
			"blockNumber":     "0x100",
			"transactionHash": "0xdeadbeef",
		}}
		b, _ := json.Marshal(logs)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(b) + `}`))
	}))
	defer ts.Close()
	e := &EVM{rpc: ts.URL, tokens: sepoliaTokens, hc: http.DefaultClient}

	pays, err := e.Received(context.Background(), "0x34A8577E4403B991Ed9A26E64514389f4C08357F", "")
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if len(pays) != 1 {
		t.Fatalf("payments = %d, want 1", len(pays))
	}
	if pays[0].Asset != "USDC" || pays[0].Amount != "2.5" {
		t.Fatalf("payment = %+v, want USDC 2.5", pays[0])
	}
	if pays[0].TxRef != "0xdeadbeef" {
		t.Fatalf("tx ref = %q", pays[0].TxRef)
	}
	// Cursor resumes at block 0x100+1 = 0x101.
	if pays[0].Cursor != "0x101" {
		t.Fatalf("cursor = %q, want 0x101", pays[0].Cursor)
	}
}

// TestBTCReceived drives Bitcoin.Received against a fake Esplora /address/{a}/txs
// that returns one tx paying the watched address; the watcher must credit BTC
// for the matching vout(s) only.
func TestBTCReceived(t *testing.T) {
	const addr = "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/address/"+addr+"/txs") {
			http.NotFound(w, r)
			return
		}
		// One tx: a 0.5 BTC vout to addr (50_000_000 sats) + an unrelated vout.
		_, _ = w.Write([]byte(`[
			{"txid":"abc123","vout":[
				{"value":50000000,"scriptpubkey_address":"` + addr + `"},
				{"value":12345,"scriptpubkey_address":"tb1qother"}
			]}
		]`))
	}))
	defer ts.Close()
	b := &Bitcoin{esplora: ts.URL, hc: http.DefaultClient}

	pays, err := b.Received(context.Background(), addr, "")
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if len(pays) != 1 {
		t.Fatalf("payments = %d, want 1", len(pays))
	}
	if pays[0].Asset != "BTC" || pays[0].Amount != "0.5" {
		t.Fatalf("payment = %+v, want BTC 0.5", pays[0])
	}
	if pays[0].TxRef != "abc123" || pays[0].Cursor != "abc123" {
		t.Fatalf("txref/cursor = %q/%q", pays[0].TxRef, pays[0].Cursor)
	}
}
