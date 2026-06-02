package custody

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"
)

// EVM testnet endpoints (Ethereum Sepolia). The RPC is a public Sepolia node;
// the faucet is web/captcha-only, so funding is manual (see ManualURL).
const (
	sepoliaRPC       = "https://ethereum-sepolia-rpc.publicnode.com"
	sepoliaFaucetURL = "https://www.alchemy.com/faucets/ethereum-sepolia"
)

// erc20Token is a tracked ERC20 contract on the EVM testnet.
type erc20Token struct {
	Symbol   string
	Contract string // 0x… contract address
	Decimals int    // display decimals
}

// sepoliaTokens are the ERC20s whose balances we report. USDC is Circle's
// official Sepolia USDC. USDT on Sepolia has no single canonical deployment, so
// it is left for the user to add a contract address (a later increment / config).
var sepoliaTokens = []erc20Token{
	{Symbol: "USDC", Contract: "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238", Decimals: 6},
}

// EVM is the Ethereum Sepolia chain: secp256k1 keys, EIP-55 addresses,
// eth_getBalance (ETH) + ERC20 balanceOf (USDC/USDT). Faucets are manual.
type EVM struct {
	rpc    string
	faucet string
	tokens []erc20Token
	hc     *http.Client
}

// NewEVM returns an EVM chain pinned to Ethereum Sepolia.
func NewEVM() *EVM {
	return &EVM{rpc: sepoliaRPC, faucet: sepoliaFaucetURL, tokens: sepoliaTokens, hc: defaultHTTPClient()}
}

func (e *EVM) ID() string      { return "eth" }
func (e *EVM) Network() string { return "sepolia" }

// NewKey generates a fresh secp256k1 private key (32 bytes).
func (e *EVM) NewKey() ([]byte, error) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return priv.Serialize(), nil
}

// Address derives the EIP-55 checksummed 0x address from a 32-byte secp256k1 key.
func (e *EVM) Address(secret []byte) (string, error) {
	if len(secret) != 32 {
		return "", fmt.Errorf("eth: bad key length %d (want 32)", len(secret))
	}
	priv := secp256k1.PrivKeyFromBytes(secret)
	pub := priv.PubKey().SerializeUncompressed() // 0x04 || X(32) || Y(32)
	hash := keccak256(pub[1:])                   // keccak of X||Y
	return eip55(hash[12:]), nil                 // last 20 bytes, checksummed
}

func keccak256(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}

// eip55 renders 20 address bytes as a mixed-case EIP-55 checksum address.
func eip55(addr20 []byte) string {
	lower := hex.EncodeToString(addr20)
	hash := keccak256([]byte(lower))
	var sb strings.Builder
	sb.WriteString("0x")
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= 'a' && c <= 'f' {
			// Uppercase the hex letter when the corresponding hash nibble >= 8.
			nibble := hash[i/2]
			if i%2 == 0 {
				nibble >>= 4
			} else {
				nibble &= 0x0f
			}
			if nibble >= 8 {
				c -= 32 // to uppercase
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// Balances returns ETH plus the configured ERC20 token balances. A token whose
// contract does not exist / reverts on this network is skipped, not fatal.
func (e *EVM) Balances(ctx context.Context, address string) ([]Balance, error) {
	var hexWei string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_getBalance", []any{address, "latest"}, &hexWei); err != nil {
		return nil, fmt.Errorf("eth: getBalance: %w", err)
	}
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(hexWei, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("eth: bad balance %q", hexWei)
	}
	out := []Balance{{Asset: "ETH", Amount: formatBigUnits(wei, 18)}}
	for _, tok := range e.tokens {
		// A non-deployed contract returns an empty 0x result → balance 0 (handled
		// in erc20Balance). A real RPC/network/decode failure is propagated rather
		// than masked as "0", so a blip can't silently under-report holdings.
		bal, err := e.erc20Balance(ctx, tok, address)
		if err != nil {
			return nil, fmt.Errorf("eth: %s balanceOf: %w", tok.Symbol, err)
		}
		out = append(out, Balance{Asset: tok.Symbol, Amount: formatBigUnits(bal, tok.Decimals)})
	}
	return out, nil
}

// erc20Balance calls balanceOf(address) on the token contract via eth_call.
func (e *EVM) erc20Balance(ctx context.Context, tok erc20Token, address string) (*big.Int, error) {
	addrBytes, err := decodeHexAddress(address)
	if err != nil {
		return nil, err
	}
	// calldata = selector(balanceOf(address)) || left-padded 32-byte address
	data := make([]byte, 0, 4+32)
	data = append(data, erc20BalanceOfSelector()...)
	padded := make([]byte, 32)
	copy(padded[12:], addrBytes)
	data = append(data, padded...)

	call := map[string]any{"to": tok.Contract, "data": "0x" + hex.EncodeToString(data)}
	var hexResult string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_call", []any{call, "latest"}, &hexResult); err != nil {
		return nil, err
	}
	raw := strings.TrimPrefix(hexResult, "0x")
	if raw == "" || isAllZeroHex(raw) {
		// Empty result (call to a non-contract) or an all-zero word → balance 0.
		return big.NewInt(0), nil
	}
	// balanceOf returns a single right-aligned 32-byte word (64 hex chars).
	if len(raw) < 64 {
		return nil, fmt.Errorf("eth: short erc20 result %q", hexResult)
	}
	v, ok := new(big.Int).SetString(raw[:64], 16)
	if !ok {
		return nil, fmt.Errorf("eth: bad erc20 balance %q", hexResult)
	}
	return v, nil
}

func isAllZeroHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// erc20BalanceOfSelector is the 4-byte selector keccak256("balanceOf(address)")[:4].
func erc20BalanceOfSelector() []byte {
	return keccak256([]byte("balanceOf(address)"))[:4]
}

// decodeHexAddress parses a 0x-prefixed 20-byte hex address (case-insensitive).
func decodeHexAddress(addr string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(addr, "0x"))
	if err != nil {
		return nil, fmt.Errorf("eth: bad address %q: %w", addr, err)
	}
	if len(b) != 20 {
		return nil, fmt.Errorf("eth: address %q is %d bytes, want 20", addr, len(b))
	}
	return b, nil
}

// Tap: Sepolia faucets are web/captcha-only, so funding is manual.
func (e *EVM) Tap(ctx context.Context, address, asset string, amount float64) (string, error) {
	return "", ErrManualFaucet
}

// ManualURL returns the Sepolia faucet to visit (ETH faucet aggregator; for
// USDC use the Circle testnet faucet).
func (e *EVM) ManualURL(asset string) (string, bool) {
	if strings.EqualFold(asset, "USDC") {
		return "https://faucet.circle.com/", true
	}
	return e.faucet, true
}
