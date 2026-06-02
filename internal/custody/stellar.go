package custody

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Stellar is the XLM testnet chain: ed25519 keys, StrKey addresses, friendbot
// faucet, and Horizon balance lookups. All endpoints are Stellar testnet.
type Stellar struct {
	horizon   string // Horizon testnet base URL
	friendbot string // friendbot faucet base URL
	hc        *http.Client
}

// NewStellar returns a Stellar chain pinned to the public testnet endpoints.
func NewStellar() *Stellar {
	return &Stellar{
		horizon:   "https://horizon-testnet.stellar.org",
		friendbot: "https://friendbot.stellar.org",
		hc:        defaultHTTPClient(),
	}
}

func (s *Stellar) ID() string      { return "xlm" }
func (s *Stellar) Network() string { return "testnet" }

// NewKey generates a fresh ed25519 seed (32 bytes) — the Stellar account secret.
func (s *Stellar) NewKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return priv.Seed(), nil
}

// Address derives the 'G…' account address from a 32-byte ed25519 seed.
func (s *Stellar) Address(secret []byte) (string, error) {
	pub, err := stellarPublic(secret)
	if err != nil {
		return "", err
	}
	return strkeyEncode(strkeyVersionAccount, pub), nil
}

// SecretKey returns the 'S…' StrKey form of the seed (for display / import into
// stock Stellar tooling). Handle with care — it unlocks the account.
func (s *Stellar) SecretKey(secret []byte) (string, error) {
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("xlm: bad seed length %d", len(secret))
	}
	return strkeyEncode(strkeyVersionSeed, secret), nil
}

func stellarPublic(secret []byte) (ed25519.PublicKey, error) {
	if len(secret) != ed25519.SeedSize {
		return nil, fmt.Errorf("xlm: bad seed length %d (want %d)", len(secret), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(secret)
	return priv.Public().(ed25519.PublicKey), nil
}

// horizonAccount is the subset of a Horizon /accounts/{id} response we read.
type horizonAccount struct {
	Balances []struct {
		Balance     string `json:"balance"`
		AssetType   string `json:"asset_type"`
		AssetCode   string `json:"asset_code"`
		AssetIssuer string `json:"asset_issuer"`
	} `json:"balances"`
}

// Balances returns the account's balances. An unfunded (not-yet-created)
// account returns no balances (Horizon 404) rather than an error.
func (s *Stellar) Balances(ctx context.Context, address string) ([]Balance, error) {
	body, err := httpGet(ctx, s.hc, s.horizon+"/accounts/"+url.PathEscape(address))
	if err != nil {
		// A brand-new account that has never been funded does not exist on
		// Horizon yet (404) — report it as simply empty, not an error.
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var acct horizonAccount
	if err := json.Unmarshal(body, &acct); err != nil {
		return nil, fmt.Errorf("xlm: decode account: %w", err)
	}
	out := make([]Balance, 0, len(acct.Balances))
	for _, b := range acct.Balances {
		asset := b.AssetCode
		if b.AssetType == "native" {
			asset = "XLM"
		}
		out = append(out, Balance{Asset: asset, Amount: b.Balance})
	}
	return out, nil
}

// Tap funds the address via friendbot (testnet only). Friendbot funds native
// XLM only; USDC is handled in a later phase. amount is ignored (friendbot
// funds a fixed amount). Returns the funding tx hash.
func (s *Stellar) Tap(ctx context.Context, address, asset string, amount float64) (string, error) {
	if asset != "" && asset != "XLM" {
		return "", ErrUnsupportedAsset
	}
	body, err := httpGet(ctx, s.hc, s.friendbot+"/?addr="+url.QueryEscape(address))
	if err != nil {
		return "", fmt.Errorf("xlm: friendbot: %w", err)
	}
	var resp struct {
		Hash   string `json:"hash"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("xlm: friendbot: decode response: %w (%s)", err, truncate(body, 200))
	}
	if resp.Hash == "" && resp.ID == "" {
		// Friendbot can return HTTP 200 with an error body (e.g. account already
		// funded, rate limited). Surface it instead of faking success.
		return "", fmt.Errorf("xlm: friendbot returned no funding tx: %s", truncate(body, 200))
	}
	if resp.Hash != "" {
		return resp.Hash, nil
	}
	return resp.ID, nil
}

// ManualURL: friendbot is automated, so XLM has no manual URL.
func (s *Stellar) ManualURL(asset string) (string, bool) {
	return "", false
}
