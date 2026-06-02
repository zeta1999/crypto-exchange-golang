package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
)

// lamportsPerSOL is the SOL base-unit scale (1 SOL = 1e9 lamports).
const lamportsPerSOL = 1_000_000_000

// defaultAirdropSOL is the amount requested when Tap is called with amount<=0.
// Public devnet caps airdrops (commonly 1–2 SOL) and rate-limits aggressively.
const defaultAirdropSOL = 1.0

// maxAirdropSOL bounds a Tap request (devnet rejects more anyway) and guards
// against a float→uint64 overflow.
const maxAirdropSOL = 10.0

// Solana is the SOL devnet chain: ed25519 keys, base58 addresses, devnet
// requestAirdrop faucet, and getBalance lookups.
type Solana struct {
	rpc string // devnet JSON-RPC endpoint
	hc  *http.Client
}

// NewSolana returns a Solana chain pinned to the public devnet RPC.
func NewSolana() *Solana {
	return &Solana{rpc: "https://api.devnet.solana.com", hc: defaultHTTPClient()}
}

func (s *Solana) ID() string      { return "sol" }
func (s *Solana) Network() string { return "devnet" }

// NewKey generates a fresh ed25519 seed (32 bytes).
func (s *Solana) NewKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return priv.Seed(), nil
}

// Address derives the base58 address (the ed25519 public key) from a seed.
func (s *Solana) Address(secret []byte) (string, error) {
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("sol: bad seed length %d (want %d)", len(secret), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(secret)
	pub := priv.Public().(ed25519.PublicKey)
	return base58Encode(pub), nil
}

// Balances returns the SOL balance via getBalance.
func (s *Solana) Balances(ctx context.Context, address string) ([]Balance, error) {
	var res struct {
		Value uint64 `json:"value"`
	}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getBalance", []any{address}, &res); err != nil {
		return nil, fmt.Errorf("sol: getBalance: %w", err)
	}
	return []Balance{{Asset: "SOL", Amount: formatUnits(res.Value, 9)}}, nil
}

// Tap requests a devnet airdrop of `amount` SOL (default 1). Returns the tx
// signature. Note: public devnet airdrops are heavily rate-limited and may
// fail with "airdrop request limit reached" — retry later or use a custom RPC.
func (s *Solana) Tap(ctx context.Context, address, asset string, amount float64) (string, error) {
	if asset != "" && asset != "SOL" {
		return "", ErrUnsupportedAsset
	}
	if amount <= 0 {
		amount = defaultAirdropSOL
	}
	if amount > maxAirdropSOL {
		return "", fmt.Errorf("sol: airdrop amount %.4f too large (devnet caps ~2 SOL)", amount)
	}
	lamports := uint64(math.Round(amount * lamportsPerSOL))
	if lamports == 0 {
		return "", fmt.Errorf("sol: airdrop amount %.10f rounds to 0 lamports", amount)
	}
	var sig string
	if err := jsonRPC(ctx, s.hc, s.rpc, "requestAirdrop", []any{address, lamports}, &sig); err != nil {
		return "", fmt.Errorf("sol: requestAirdrop: %w", err)
	}
	return sig, nil
}

// ManualURL: devnet airdrop is automated, so SOL has no manual URL.
func (s *Solana) ManualURL(asset string) (string, bool) {
	return "", false
}
