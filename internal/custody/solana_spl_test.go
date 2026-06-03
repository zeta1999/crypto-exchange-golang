package custody

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSolanaRPC routes JSON-RPC methods to canned results. getTokenAccountsByOwner
// returns a token account only for owners in `tokenAccounts` (owner pubkey ->
// token-account pubkey), letting a test model "recipient has no USDC account".
type fakeSolanaRPC struct {
	tokenAccounts map[string]string // owner base58 -> token-account base58
	sent          string            // last base64 tx submitted
}

func (f *fakeSolanaRPC) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		result := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(b) + `}`))
		}
		switch req.Method {
		case "getLatestBlockhash":
			// 32 zero bytes base58-encodes to "11111111111111111111111111111111".
			result(map[string]any{"value": map[string]any{"blockhash": "11111111111111111111111111111111"}})
		case "getTokenAccountsByOwner":
			owner, _ := req.Params[0].(string)
			if ta, ok := f.tokenAccounts[owner]; ok {
				result(map[string]any{"value": []any{map[string]any{"pubkey": ta}}})
			} else {
				result(map[string]any{"value": []any{}})
			}
		case "sendTransaction":
			f.sent, _ = req.Params[0].(string)
			result("sig-" + f.sent[:8])
		default:
			result(nil)
		}
	}
}

// A valid 32-byte base58 pubkey (32 zero bytes) to stand in for token accounts.
const fakeTokenAcct = "11111111111111111111111111111111"

func TestSendSPL_USDC(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	seed := priv.Seed()
	s := &Solana{hc: http.DefaultClient}
	ownerAddr, _ := s.Address(seed)
	dest := "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM" // arbitrary valid base58 owner

	fake := &fakeSolanaRPC{tokenAccounts: map[string]string{
		ownerAddr: fakeTokenAcct,
		dest:      fakeTokenAcct,
	}}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()
	s.rpc = ts.URL

	sig, err := s.Send(context.Background(), seed, "USDC", dest, "1.5")
	if err != nil {
		t.Fatalf("USDC send: %v", err)
	}
	if !strings.HasPrefix(sig, "sig-") || fake.sent == "" {
		t.Fatalf("expected a submitted tx, got sig=%q sent=%q", sig, fake.sent)
	}
}

func TestSendSPL_RecipientNoAccount(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	seed := priv.Seed()
	s := &Solana{hc: http.DefaultClient}
	ownerAddr, _ := s.Address(seed)
	dest := "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

	// Only the sender has a token account; the recipient does not.
	fake := &fakeSolanaRPC{tokenAccounts: map[string]string{ownerAddr: fakeTokenAcct}}
	ts := httptest.NewServer(fake.handler())
	defer ts.Close()
	s.rpc = ts.URL

	_, err := s.Send(context.Background(), seed, "USDC", dest, "1.5")
	if err == nil || !strings.Contains(err.Error(), "no token account") {
		t.Fatalf("want a 'no token account' error, got %v", err)
	}
	if fake.sent != "" {
		t.Fatalf("must not submit a tx when the recipient has no account")
	}
}
