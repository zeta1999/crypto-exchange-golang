package custody

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- keystore ---

func TestKeystoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ks.json")
	ks, err := Open(path, "correct horse battery staple")
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	secret := []byte("a-32-byte-seed-aaaaaaaaaaaaaaaaa")
	if err := ks.Put("alice", "xlm", "GADDR", secret); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Reopen with the correct passphrase and read the secret back.
	ks2, err := Open(path, "correct horse battery staple")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	chain, addr, got, err := ks2.Get("alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if chain != "xlm" || addr != "GADDR" || !bytes.Equal(got, secret) {
		t.Fatalf("round-trip mismatch: chain=%q addr=%q secret=%q", chain, addr, got)
	}

	// The on-disk file must not contain the plaintext secret.
	if data := mustRead(t, path); bytes.Contains(data, secret) {
		t.Fatal("plaintext secret found on disk")
	}
}

func TestKeystoreWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ks.json")
	if _, err := Open(path, "right"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Open(path, "wrong"); err != ErrBadPassphrase {
		t.Fatalf("wrong passphrase err = %v, want ErrBadPassphrase", err)
	}
}

func TestKeystoreListAndDelete(t *testing.T) {
	ks, err := Open(filepath.Join(t.TempDir(), "ks.json"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	_ = ks.Put("b", "sol", "addrB", []byte("s2"))
	_ = ks.Put("a", "xlm", "addrA", []byte("s1"))
	list := ks.List()
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("list (want sorted a,b) = %+v", list)
	}
	if list[0].Secret != "" {
		t.Error("List must not expose secrets")
	}
	if err := ks.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ks.Get("a"); err == nil {
		t.Error("get after delete should fail")
	}
}

// TestKeystoreRejectsTamperedIters ensures a downgraded KDF cost in the file is
// refused on open (so an attacker can't cheapen an offline passphrase attack).
func TestKeystoreRejectsTamperedIters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ks.json")
	if _, err := Open(path, "pw"); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, path)
	tampered := bytes.Replace(data, []byte(`"iters": 600000`), []byte(`"iters": 1`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("test setup: iters field not found to tamper")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "pw"); err == nil {
		t.Fatal("Open must reject an implausibly low iters count")
	}
}

// TestStellarFaucetEmptyBodyErrors ensures a friendbot HTTP 200 with no funding
// tx is reported as an error, not a fake success with an empty ref.
func TestStellarFaucetEmptyBodyErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"detail":"account already funded"}`))
	}))
	defer ts.Close()
	s := NewStellar()
	s.friendbot = ts.URL // in-package: point at the fake faucet
	if _, err := s.Tap(context.Background(), "GADDR", "", 0); err == nil {
		t.Fatal("empty friendbot body must produce an error")
	}
}

func TestSolanaAirdropBounds(t *testing.T) {
	t.Setenv("CIRCLE_API_KEY", "")
	s := NewSolana()
	if _, err := s.Tap(context.Background(), "addr", "", 1000); err == nil {
		t.Error("oversized airdrop should be rejected before any RPC call")
	}
	// A truly unsupported asset is rejected; USDC routes to Circle (manual w/o key).
	if _, err := s.Tap(context.Background(), "addr", "DOGE", 1); err != ErrUnsupportedAsset {
		t.Errorf("unknown asset err = %v, want ErrUnsupportedAsset", err)
	}
	if _, err := s.Tap(context.Background(), "addr", "USDC", 0); err != ErrManualFaucet {
		t.Errorf("USDC tap without key = %v, want ErrManualFaucet", err)
	}
}

// --- StrKey (Stellar) ---

func TestStrKeyRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 32)
	enc := strkeyEncode(strkeyVersionAccount, payload)
	if !strings.HasPrefix(enc, "G") {
		t.Errorf("account StrKey should start with G, got %q", enc[:1])
	}
	dec, err := strkeyDecode(strkeyVersionAccount, enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec, payload) {
		t.Fatalf("round-trip payload mismatch")
	}
	// Seed version → 'S'.
	if s := strkeyEncode(strkeyVersionSeed, payload); !strings.HasPrefix(s, "S") {
		t.Errorf("seed StrKey should start with S, got %q", s[:1])
	}
	// Tampering the last char breaks the checksum.
	bad := enc[:len(enc)-1] + string(flipBase32(enc[len(enc)-1]))
	if _, err := strkeyDecode(strkeyVersionAccount, bad); err == nil {
		t.Error("tampered StrKey should fail checksum")
	}
}

// TestStrKeyDecodesRealStellarAddress validates the StrKey codec (base32 +
// version + CRC16) against a REAL Stellar testnet address (the Circle USDC
// testnet issuer). A wrong CRC/endianness/alphabet would fail this.
func TestStrKeyDecodesRealStellarAddress(t *testing.T) {
	const usdcIssuer = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	raw, err := strkeyDecode(strkeyVersionAccount, usdcIssuer)
	if err != nil {
		t.Fatalf("decode real address: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded len = %d, want 32", len(raw))
	}
	if got := strkeyEncode(strkeyVersionAccount, raw); got != usdcIssuer {
		t.Fatalf("re-encode mismatch:\n got %q\nwant %q", got, usdcIssuer)
	}
}

// --- base58 (Solana) ---

func TestBase58RoundTrip(t *testing.T) {
	// 32 zero bytes encode to 32 '1's (leading-zero preservation).
	if got := base58Encode(make([]byte, 32)); got != strings.Repeat("1", 32) {
		t.Fatalf("zero-bytes base58 = %q", got)
	}
	if got := base58Encode([]byte{0, 0, 1}); got != "112" {
		t.Fatalf("base58({0,0,1}) = %q, want 112", got)
	}
	payload := bytes.Repeat([]byte{0x9C, 0x01}, 16)
	dec, err := base58Decode(base58Encode(payload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec, payload) {
		t.Fatal("base58 round-trip mismatch")
	}
}

// TestBase58DecodesRealSolanaMint validates base58 against a REAL Solana devnet
// address (the Circle USDC devnet mint) — 32 bytes, alphabet-correct.
func TestBase58DecodesRealSolanaMint(t *testing.T) {
	const usdcMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	raw, err := base58Decode(usdcMint)
	if err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded len = %d, want 32", len(raw))
	}
	if got := base58Encode(raw); got != usdcMint {
		t.Fatalf("re-encode mismatch: got %q want %q", got, usdcMint)
	}
}

func TestFormatUnits(t *testing.T) {
	cases := []struct {
		v   uint64
		dec int
		out string
	}{
		{1_500_000_000, 9, "1.5"},
		{1_000_000_000, 9, "1"},
		{1, 9, "0.000000001"},
		{0, 9, "0"},
		{123, 0, "123"},
	}
	for _, c := range cases {
		if got := formatUnits(c.v, c.dec); got != c.out {
			t.Errorf("formatUnits(%d,%d) = %q, want %q", c.v, c.dec, got, c.out)
		}
	}
}

// --- chains: keygen + address ---

func TestStellarKeygenDeterministic(t *testing.T) {
	s := NewStellar()
	seed, err := s.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("seed len = %d", len(seed))
	}
	a1, err := s.Address(seed)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.Address(seed)
	if a1 != a2 || !strings.HasPrefix(a1, "G") {
		t.Fatalf("address not deterministic/valid: %q vs %q", a1, a2)
	}
	// Secret StrKey ('S…') must round-trip back to the seed.
	sk, err := s.SecretKey(seed)
	if err != nil || !strings.HasPrefix(sk, "S") {
		t.Fatalf("secret key %q err %v", sk, err)
	}
	back, err := strkeyDecode(strkeyVersionSeed, sk)
	if err != nil || !bytes.Equal(back, seed) {
		t.Fatalf("secret key did not round-trip to seed")
	}
}

func TestSolanaKeygenDeterministic(t *testing.T) {
	s := NewSolana()
	seed, err := s.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	a1, err := s.Address(seed)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.Address(seed)
	if a1 != a2 {
		t.Fatal("address not deterministic")
	}
	// Address must base58-decode to the 32-byte ed25519 public key.
	raw, err := base58Decode(a1)
	if err != nil || len(raw) != 32 {
		t.Fatalf("address not a 32-byte base58 key: %v", err)
	}
}

// --- testnet guard ---

// fakeMainnet is a minimal Chain that declares a forbidden network.
type fakeMainnet struct{}

func (fakeMainnet) ID() string                                          { return "evil" }
func (fakeMainnet) Network() string                                     { return "mainnet" }
func (fakeMainnet) NewKey() ([]byte, error)                             { return nil, nil }
func (fakeMainnet) Address([]byte) (string, error)                      { return "", nil }
func (fakeMainnet) Balances(context.Context, string) ([]Balance, error) { return nil, nil }

func TestMustTestnetPanicsOnMainnet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustTestnet should panic on a mainnet chain")
		}
	}()
	MustTestnet(fakeMainnet{})
}

// --- helpers ---

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// flipBase32 returns a different valid base32 char so a tampered StrKey stays
// decodable but fails the checksum.
func flipBase32(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}
