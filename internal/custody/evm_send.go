package custody

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// sepoliaChainID is the EIP-155 chain id for Ethereum Sepolia.
var sepoliaChainID = big.NewInt(11155111)

// --- minimal RLP encoding (BIP for Ethereum tx) ---

// rlpStr encodes a byte string per RLP.
func rlpStr(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	if len(b) <= 55 {
		return append([]byte{0x80 + byte(len(b))}, b...)
	}
	l := bigEndian(uint64(len(b)))
	return append(append([]byte{0xb7 + byte(len(l))}, l...), b...)
}

// rlpList wraps already-encoded items in an RLP list header.
func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	if len(payload) <= 55 {
		return append([]byte{0xc0 + byte(len(payload))}, payload...)
	}
	l := bigEndian(uint64(len(payload)))
	return append(append([]byte{0xf7 + byte(len(l))}, l...), payload...)
}

// bigEndian is the minimal big-endian encoding of a uint64 (no leading zeros).
func bigEndian(n uint64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return b[i:]
}

// rlpInt encodes a non-negative big.Int as a minimal RLP string (0 → empty).
func rlpInt(n *big.Int) []byte { return rlpStr(n.Bytes()) }

// signLegacyTx builds and signs an EIP-155 legacy transaction, returning the
// 0x-prefixed raw-tx hex for eth_sendRawTransaction. to is 20 bytes; data may be
// empty (native transfer) or ERC20 calldata.
func signLegacyTx(priv *secp256k1.PrivateKey, chainID, nonce, gasPrice, gasLimit, value *big.Int, to, data []byte) (string, error) {
	// Signing payload: rlp([nonce,gasPrice,gasLimit,to,value,data,chainID,0,0]).
	sigData := rlpList(
		rlpInt(nonce), rlpInt(gasPrice), rlpInt(gasLimit),
		rlpStr(to), rlpInt(value), rlpStr(data),
		rlpInt(chainID), rlpStr(nil), rlpStr(nil),
	)
	hash := keccak256(sigData)

	// SignCompact returns [V(27+recid)][R(32)][S(32)] with canonical low-S.
	sig := ecdsa.SignCompact(priv, hash, false)
	recid := int64(sig[0] - 27)
	r := new(big.Int).SetBytes(sig[1:33])
	s := new(big.Int).SetBytes(sig[33:65])
	// EIP-155: v = recid + chainID*2 + 35.
	v := new(big.Int).Add(big.NewInt(recid+35), new(big.Int).Mul(chainID, big.NewInt(2)))

	raw := rlpList(
		rlpInt(nonce), rlpInt(gasPrice), rlpInt(gasLimit),
		rlpStr(to), rlpInt(value), rlpStr(data),
		rlpInt(v), rlpInt(r), rlpInt(s),
	)
	return "0x" + hex.EncodeToString(raw), nil
}

// Send signs and broadcasts a Sepolia payment of asset (native "ETH" or a
// configured ERC20 like "USDC"/"USDT") from secret to destAddr. amount is a
// decimal string in display units. Returns the tx hash.
func (e *EVM) Send(ctx context.Context, secret []byte, asset, destAddr, amount string) (string, error) {
	if len(secret) != 32 {
		return "", fmt.Errorf("eth: bad key length %d", len(secret))
	}
	priv := secp256k1.PrivKeyFromBytes(secret)
	from, err := e.Address(secret)
	if err != nil {
		return "", err
	}
	toBytes, err := decodeHexAddress(destAddr)
	if err != nil {
		return "", err
	}

	var to []byte
	var value *big.Int
	var data []byte
	var gasLimit *big.Int
	if asset == "" || asset == "ETH" {
		wei, err := baseUnits(amount, 18)
		if err != nil {
			return "", fmt.Errorf("eth: amount: %w", err)
		}
		to, value, data, gasLimit = toBytes, wei, nil, big.NewInt(21000)
	} else {
		tok, ok := e.token(asset)
		if !ok {
			return "", ErrUnsupportedAsset
		}
		contract, err := decodeHexAddress(tok.Contract)
		if err != nil {
			return "", err
		}
		units, err := baseUnits(amount, tok.Decimals)
		if err != nil {
			return "", fmt.Errorf("eth: amount: %w", err)
		}
		// transfer(address,uint256): selector || pad(to,32) || pad(amount,32)
		data = make([]byte, 0, 4+64)
		data = append(data, keccak256([]byte("transfer(address,uint256)"))[:4]...)
		data = append(data, leftPad32(toBytes)...)
		data = append(data, leftPad32(units.Bytes())...)
		to, value, gasLimit = contract, big.NewInt(0), big.NewInt(90000)
	}

	nonce, err := e.txCount(ctx, from)
	if err != nil {
		return "", err
	}
	gasPrice, err := e.gasPrice(ctx)
	if err != nil {
		return "", err
	}
	rawTx, err := signLegacyTx(priv, sepoliaChainID, nonce, gasPrice, gasLimit, value, to, data)
	if err != nil {
		return "", err
	}
	var txHash string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_sendRawTransaction", []any{rawTx}, &txHash); err != nil {
		return "", fmt.Errorf("eth: sendRawTransaction: %w", err)
	}
	return txHash, nil
}

// LatestCursor returns the current block number (hex), so the deposit watcher
// only sees NEW transfers.
func (e *EVM) LatestCursor(ctx context.Context, address string) (string, error) {
	var blk string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_blockNumber", nil, &blk); err != nil {
		return "", fmt.Errorf("eth: blockNumber: %w", err)
	}
	return blk, nil
}

// Received reports incoming ERC20 deposits (Transfer events TO the address) for
// the configured tokens since `cursor` (a block number). NATIVE-ETH deposits are
// not detected — without an indexer there is no per-tx incoming feed for a
// wallet that also sends; for arb rebalancing the stablecoins (USDC/USDT) are
// what matter, and they emit Transfer logs.
func (e *EVM) Received(ctx context.Context, address, cursor string) ([]Payment, error) {
	if len(e.tokens) == 0 {
		return nil, nil
	}
	addrBytes, err := decodeHexAddress(address)
	if err != nil {
		return nil, err
	}
	contracts := make([]string, 0, len(e.tokens))
	byContract := make(map[string]erc20Token, len(e.tokens))
	for _, t := range e.tokens {
		contracts = append(contracts, t.Contract)
		byContract[strings.ToLower(t.Contract)] = t
	}
	fromBlock := cursor
	if fromBlock == "" {
		fromBlock = "0x0"
	}
	transferSig := "0x" + hex.EncodeToString(keccak256([]byte("Transfer(address,uint256)")))
	addrTopic := "0x" + hex.EncodeToString(leftPad32(addrBytes))
	filter := map[string]any{
		"fromBlock": fromBlock,
		"toBlock":   "latest",
		"address":   contracts,
		"topics":    []any{transferSig, nil, addrTopic}, // topic2 = indexed recipient
	}
	var logs []struct {
		Address     string `json:"address"`
		Data        string `json:"data"`
		BlockNumber string `json:"blockNumber"`
		TxHash      string `json:"transactionHash"`
	}
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_getLogs", []any{filter}, &logs); err != nil {
		return nil, fmt.Errorf("eth: getLogs: %w", err)
	}
	var out []Payment
	for _, lg := range logs {
		tok, ok := byContract[strings.ToLower(lg.Address)]
		if !ok {
			continue
		}
		amt, ok := new(big.Int).SetString(strings.TrimPrefix(lg.Data, "0x"), 16)
		if !ok {
			continue
		}
		blk, err := hexToBig(lg.BlockNumber)
		if err != nil {
			continue
		}
		out = append(out, Payment{
			TxRef:  lg.TxHash,
			Asset:  tok.Symbol,
			Amount: formatBigUnits(amt, tok.Decimals),
			Cursor: "0x" + new(big.Int).Add(blk, big.NewInt(1)).Text(16), // resume after this block
		})
	}
	return out, nil
}

func (e *EVM) token(symbol string) (erc20Token, bool) {
	for _, t := range e.tokens {
		if t.Symbol == symbol {
			return t, true
		}
	}
	return erc20Token{}, false
}

func (e *EVM) txCount(ctx context.Context, address string) (*big.Int, error) {
	var hexN string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_getTransactionCount", []any{address, "pending"}, &hexN); err != nil {
		return nil, fmt.Errorf("eth: getTransactionCount: %w", err)
	}
	return hexToBig(hexN)
}

func (e *EVM) gasPrice(ctx context.Context) (*big.Int, error) {
	var hexP string
	if err := jsonRPC(ctx, e.hc, e.rpc, "eth_gasPrice", nil, &hexP); err != nil {
		return nil, fmt.Errorf("eth: gasPrice: %w", err)
	}
	return hexToBig(hexP)
}

func hexToBig(h string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(strings.TrimPrefix(h, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("eth: bad hex int %q", h)
	}
	return n, nil
}

func leftPad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// baseUnits converts a decimal-string amount to integer base units (e.g. wei):
// it pads/truncates the fractional part to `dec` digits and concatenates. A
// negative amount is rejected; extra fractional digits are truncated.
func baseUnits(amount string, dec int) (*big.Int, error) {
	if strings.HasPrefix(amount, "-") {
		return nil, fmt.Errorf("negative amount %q", amount)
	}
	intPart, frac := amount, ""
	if i := strings.IndexByte(amount, '.'); i >= 0 {
		intPart, frac = amount[:i], amount[i+1:]
	}
	if len(frac) > dec {
		frac = frac[:dec]
	}
	for len(frac) < dec {
		frac += "0"
	}
	combined := strings.TrimLeft(intPart+frac, "0")
	if combined == "" {
		combined = "0"
	}
	n, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("bad amount %q", amount)
	}
	return n, nil
}
