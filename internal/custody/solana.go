package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
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
	out := []Balance{{Asset: "SOL", Amount: formatUnits(res.Value, 9)}}
	// USDC (SPL) balance: sum any token accounts the owner holds for the mint.
	if usdc, ok, err := s.splBalance(ctx, address, usdcSolanaMint); err != nil {
		return nil, err
	} else if ok {
		out = append(out, Balance{Asset: "USDC", Amount: usdc})
	}
	return out, nil
}

// splBalance returns the owner's total balance of an SPL token (by mint), as a
// UI-amount string. ok is false when the owner holds no token account for the
// mint (so the asset is simply omitted rather than shown as 0).
func (s *Solana) splBalance(ctx context.Context, owner, mint string) (string, bool, error) {
	var res struct {
		Value []struct {
			Account struct {
				Data struct {
					Parsed struct {
						Info struct {
							TokenAmount struct {
								Amount      string `json:"amount"`
								Decimals    int    `json:"decimals"`
								UIAmountStr string `json:"uiAmountString"`
							} `json:"tokenAmount"`
						} `json:"info"`
					} `json:"parsed"`
				} `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}
	params := []any{owner, map[string]any{"mint": mint}, map[string]any{"encoding": "jsonParsed"}}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getTokenAccountsByOwner", params, &res); err != nil {
		return "", false, fmt.Errorf("sol: getTokenAccountsByOwner: %w", err)
	}
	if len(res.Value) == 0 {
		return "", false, nil
	}
	// Sum across token accounts (usually one ATA) using big.Int base units.
	total := new(big.Int)
	dec := 0
	for _, v := range res.Value {
		ta := v.Account.Data.Parsed.Info.TokenAmount
		dec = ta.Decimals
		n, ok := new(big.Int).SetString(ta.Amount, 10)
		if !ok {
			return "", false, fmt.Errorf("sol: unparseable token amount %q", ta.Amount)
		}
		total.Add(total, n)
	}
	return formatBigUnits(total, dec), true, nil
}

// Tap requests a devnet airdrop of `amount` SOL (default 1). Returns the tx
// signature. Note: public devnet airdrops are heavily rate-limited and may
// fail with "airdrop request limit reached" — retry later or use a custom RPC.
func (s *Solana) Tap(ctx context.Context, address, asset string, amount float64) (string, error) {
	if asset == "USDC" {
		// Circle drips USDC and auto-creates the recipient's token account.
		// SOL-DEVNET is Circle's verified devnet id; override via env if needed.
		return circleDrip(ctx, s.hc, envOr("CIRCLE_SOL_BLOCKCHAIN", "SOL-DEVNET"), address)
	}
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

// ManualURL: devnet airdrop automates SOL; USDC falls back to Circle's web
// faucet when no API key is configured.
func (s *Solana) ManualURL(asset string) (string, bool) {
	if asset == "USDC" {
		return circleManualURL, true
	}
	return "", false
}
