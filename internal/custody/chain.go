// Package custody is a TESTNET-ONLY toolkit for creating crypto wallets and
// funding them from faucets, with the wallet secret encrypted at rest. It is a
// standalone developer / test-bed utility (driven by cmd/custody) and is
// deliberately NOT wired into the exchange server.
//
// Safety: it must never touch mainnet. Every Chain pins testnet endpoints and
// declares a testnet Network(); the registry calls MustTestnet so a non-testnet
// network fails loudly at startup rather than silently moving real funds.
package custody

import (
	"context"
	"errors"
	"sort"
)

// Balance is a single asset balance held by an address, as a human-readable
// decimal string in the asset's display units (e.g. "9999.9999900" XLM).
type Balance struct {
	Asset  string // "XLM", "SOL", "USDC", "ETH", "USDT", ...
	Amount string
}

// Chain is a pluggable testnet chain: key generation, address derivation, and
// balance lookup. Implementations pin testnet endpoints (overridable only to a
// different testnet endpoint, e.g. for tests).
type Chain interface {
	ID() string      // short id: "xlm", "sol", "eth", "btc"
	Network() string // testnet network id: "testnet", "devnet", "sepolia", "signet"

	// NewKey generates a fresh private key / seed (raw bytes, chain-specific).
	NewKey() (secret []byte, err error)
	// Address derives the public deposit address from a secret produced by NewKey.
	Address(secret []byte) (string, error)
	// Balances returns the address's balances across the assets this chain tracks.
	Balances(ctx context.Context, address string) ([]Balance, error)
}

// Faucet is an optional Chain capability: fund a testnet address. Tap performs
// an automated drip and returns a reference (tx hash / signature) when it can;
// when a faucet is web/captcha-only, Tap returns ErrManualFaucet and ManualURL
// gives the human faucet URL to visit.
type Faucet interface {
	Tap(ctx context.Context, address, asset string, amount float64) (ref string, err error)
	ManualURL(asset string) (url string, ok bool)
}

var (
	// ErrUnknownChain is returned by Registry.Get for an unregistered id.
	ErrUnknownChain = errors.New("custody: unknown chain")
	// ErrNoFaucet means the chain exposes no faucet at all.
	ErrNoFaucet = errors.New("custody: chain has no faucet")
	// ErrManualFaucet means funding requires visiting a web faucet (see ManualURL).
	ErrManualFaucet = errors.New("custody: faucet is manual; visit the faucet URL")
	// ErrUnsupportedAsset means the chain/faucet does not handle the named asset.
	ErrUnsupportedAsset = errors.New("custody: unsupported asset for this chain")
)

// testnetNetworks is the allow-list of network ids a Chain may declare.
var testnetNetworks = map[string]bool{
	"testnet": true, // Stellar
	"devnet":  true, // Solana
	"sepolia": true, // Ethereum
	"signet":  true, // Bitcoin
}

// MustTestnet asserts c targets a testnet network and panics otherwise, so a
// mainnet endpoint can never be wired in by mistake.
func MustTestnet(c Chain) Chain {
	if !testnetNetworks[c.Network()] {
		panic("custody: chain " + c.ID() + " declares non-testnet network " + c.Network())
	}
	return c
}

// Registry holds the available chains by id.
type Registry struct {
	chains map[string]Chain
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{chains: make(map[string]Chain)}
}

// Register adds c (after asserting it is testnet). A later registration with the
// same id replaces the earlier one.
func (r *Registry) Register(c Chain) {
	r.chains[c.ID()] = MustTestnet(c)
}

// Get returns the chain for id or ErrUnknownChain.
func (r *Registry) Get(id string) (Chain, error) {
	c, ok := r.chains[id]
	if !ok {
		return nil, ErrUnknownChain
	}
	return c, nil
}

// IDs returns the registered chain ids, sorted.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.chains))
	for id := range r.chains {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
