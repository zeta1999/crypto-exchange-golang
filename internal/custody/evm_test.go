package custody

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEVMAddressVector checks secp256k1 → keccak → address derivation against
// the canonical vector: private key = 1 yields Ethereum address
// 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf.
func TestEVMAddressVector(t *testing.T) {
	secret := make([]byte, 32)
	secret[31] = 1 // big-endian private key value 1
	e := NewEVM()
	addr, err := e.Address(secret)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	const want = "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf"
	if !strings.EqualFold(addr, want) {
		t.Fatalf("privkey=1 address = %s, want %s", addr, want)
	}
}

// TestEIP55Checksum validates the mixed-case checksum against the EIP-55 spec
// examples (https://eips.ethereum.org/EIPS/eip-55).
func TestEIP55Checksum(t *testing.T) {
	cases := []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	}
	for _, want := range cases {
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(want), "0x"))
		if err != nil {
			t.Fatalf("decode %s: %v", want, err)
		}
		if got := eip55(raw); got != want {
			t.Errorf("eip55 = %s, want %s", got, want)
		}
	}
}

// TestERC20Selector pins the balanceOf selector (validates keccak256).
func TestERC20Selector(t *testing.T) {
	if got := hex.EncodeToString(erc20BalanceOfSelector()); got != "70a08231" {
		t.Fatalf("balanceOf selector = %s, want 70a08231", got)
	}
}

// TestEVMBalancesParsing drives Balances against a fake JSON-RPC server,
// verifying wei→ETH (big.Int) and ERC20 balanceOf → display units.
func TestEVMBalancesParsing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := readAll(r)
		_ = json.Unmarshal(body, &req)
		var result string
		switch req.Method {
		case "eth_getBalance":
			result = "0x429d069189e0000" // 0.3 ETH in wei (3e17)
		case "eth_call":
			// 1500000 (1.5 USDC at 6 decimals) as a 32-byte hex word
			result = "0x" + strings.Repeat("0", 58) + "16e360"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + result + `"}`))
	}))
	defer ts.Close()

	e := NewEVM()
	e.rpc = ts.URL // in-package: point at the fake RPC

	bals, err := e.Balances(context.Background(), "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf")
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	got := map[string]string{}
	for _, b := range bals {
		got[b.Asset] = b.Amount
	}
	if got["ETH"] != "0.3" {
		t.Errorf("ETH = %q, want 0.3", got["ETH"])
	}
	if got["USDC"] != "1.5" {
		t.Errorf("USDC = %q, want 1.5", got["USDC"])
	}
}

func TestEVMKeygen(t *testing.T) {
	e := NewEVM()
	k, err := e.NewKey()
	if err != nil || len(k) != 32 {
		t.Fatalf("NewKey len=%d err=%v", len(k), err)
	}
	a1, err := e.Address(k)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := e.Address(k)
	if a1 != a2 || !strings.HasPrefix(a1, "0x") || len(a1) != 42 {
		t.Fatalf("address not deterministic/valid: %q", a1)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
