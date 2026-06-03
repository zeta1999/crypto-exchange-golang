package custody

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// compactU16 is Solana's short-vec length prefix. We only emit values < 128
// (single byte), which covers every count in a simple transfer (≤3 accounts,
// 1 instruction, 2 indices, 12-byte data).
func compactU16(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	// >=128 would need continuation bytes; our messages never reach it.
	return []byte{byte(n&0x7f) | 0x80, byte(n >> 7)}
}

// solTransferMessage serializes a legacy Solana message for a single System
// Program transfer of `lamports` from→to, against recentBlockhash (32 bytes).
// Account order: [from(signer,writable), to(writable), SystemProgram(readonly)].
func solTransferMessage(from, to, recentBlockhash []byte, lamports uint64) []byte {
	systemProgram := make([]byte, 32) // all-zero = "11111111111111111111111111111111"

	// instruction data: u32 LE instruction index 2 (Transfer) || u64 LE lamports.
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data[0:4], 2)
	binary.LittleEndian.PutUint64(data[4:12], lamports)

	var m []byte
	m = append(m, 1, 0, 1) // header: 1 required sig, 0 readonly-signed, 1 readonly-unsigned
	m = append(m, compactU16(3)...)
	m = append(m, from...)
	m = append(m, to...)
	m = append(m, systemProgram...)
	m = append(m, recentBlockhash...)
	m = append(m, compactU16(1)...) // 1 instruction
	m = append(m, 2)                // programIdIndex = SystemProgram (account #2)
	m = append(m, compactU16(2)...) // 2 account indices
	m = append(m, 0, 1)             // from, to
	m = append(m, compactU16(len(data))...)
	m = append(m, data...)
	return m
}

// Send signs and broadcasts a Solana devnet SOL transfer. (SPL-token transfers
// are a follow-up.) amount is a decimal SOL string. Returns the tx signature.
func (s *Solana) Send(ctx context.Context, secret []byte, asset, destAddr, amount string) (string, error) {
	if asset != "" && asset != "SOL" {
		return "", ErrUnsupportedAsset
	}
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("sol: bad seed length %d", len(secret))
	}
	priv := ed25519.NewKeyFromSeed(secret)
	from := []byte(priv.Public().(ed25519.PublicKey))
	to, err := base58Decode(destAddr)
	if err != nil || len(to) != 32 {
		return "", fmt.Errorf("sol: bad destination address")
	}
	lam, err := baseUnits(amount, 9)
	if err != nil {
		return "", fmt.Errorf("sol: amount: %w", err)
	}
	if !lam.IsUint64() {
		return "", fmt.Errorf("sol: amount too large")
	}

	var bh struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getLatestBlockhash", []any{map[string]any{"commitment": "finalized"}}, &bh); err != nil {
		return "", fmt.Errorf("sol: getLatestBlockhash: %w", err)
	}
	blockhash, err := base58Decode(bh.Value.Blockhash)
	if err != nil || len(blockhash) != 32 {
		return "", fmt.Errorf("sol: bad blockhash")
	}

	msg := solTransferMessage(from, to, blockhash, lam.Uint64())
	sig := ed25519.Sign(priv, msg)
	tx := append([]byte{1}, sig...) // 1 signature
	tx = append(tx, msg...)
	b64 := base64.StdEncoding.EncodeToString(tx)

	var txSig string
	if err := jsonRPC(ctx, s.hc, s.rpc, "sendTransaction", []any{b64, map[string]any{"encoding": "base64"}}, &txSig); err != nil {
		return "", fmt.Errorf("sol: sendTransaction: %w", err)
	}
	return txSig, nil
}

// LatestCursor returns the address's most recent transaction signature so a
// fresh watcher only sees newer ones.
func (s *Solana) LatestCursor(ctx context.Context, address string) (string, error) {
	var sigs []struct {
		Signature string `json:"signature"`
	}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getSignaturesForAddress", []any{address, map[string]any{"limit": 1}}, &sigs); err != nil {
		return "", fmt.Errorf("sol: getSignaturesForAddress: %w", err)
	}
	if len(sigs) == 0 {
		return "", nil
	}
	return sigs[0].Signature, nil
}

// Received reports incoming SOL deposits since `cursor` (a tx signature). For
// each newer signature it inspects the tx's pre/post balances for the address;
// a positive delta is a deposit (a send shows negative → skipped).
func (s *Solana) Received(ctx context.Context, address, cursor string) ([]Payment, error) {
	params := []any{address, map[string]any{"limit": 25}}
	if cursor != "" {
		params = []any{address, map[string]any{"limit": 25, "until": cursor}}
	}
	var sigs []struct {
		Signature string `json:"signature"`
	}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getSignaturesForAddress", params, &sigs); err != nil {
		return nil, fmt.Errorf("sol: getSignaturesForAddress: %w", err)
	}
	// Returned newest-first; process oldest-first so the cursor advances forward.
	var out []Payment
	for i := len(sigs) - 1; i >= 0; i-- {
		sig := sigs[i].Signature
		delta, ok, err := s.balanceDelta(ctx, sig, address)
		if err != nil || !ok || delta <= 0 {
			// Not an incoming deposit (a send, fee-only, etc.): advance the cursor
			// past it with a zero-amount payment (Credit is a no-op on 0).
			out = append(out, Payment{TxRef: sig, Asset: "SOL", Amount: "0", Cursor: sig})
			continue
		}
		out = append(out, Payment{
			TxRef:  sig,
			Asset:  "SOL",
			Amount: formatUnits(uint64(delta), 9),
			Cursor: sig,
		})
	}
	return out, nil
}

// balanceDelta returns the address's lamport change in tx sig (post - pre).
func (s *Solana) balanceDelta(ctx context.Context, sig, address string) (int64, bool, error) {
	var tx struct {
		Meta struct {
			PreBalances  []int64 `json:"preBalances"`
			PostBalances []int64 `json:"postBalances"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []string `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	cfg := map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getTransaction", []any{sig, cfg}, &tx); err != nil {
		return 0, false, err
	}
	for i, k := range tx.Transaction.Message.AccountKeys {
		if k == address && i < len(tx.Meta.PreBalances) && i < len(tx.Meta.PostBalances) {
			return tx.Meta.PostBalances[i] - tx.Meta.PreBalances[i], true, nil
		}
	}
	return 0, false, nil
}
