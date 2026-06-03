package custody

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
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

// splTokenProgramID is the SPL Token program (base58). Token-account
// instructions (TransferChecked) name it as their program id.
const splTokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// usdcSolanaDecimals is USDC's SPL mint precision (6) — required by
// TransferChecked, which re-validates the amount against the mint's decimals.
const usdcSolanaDecimals = 6

// splTransferMessage serializes a legacy Solana message for a single SPL
// TransferChecked from→dest token account, authorised by owner, against
// recentBlockhash. amount is in the mint's base units; decimals is the mint
// precision (TransferChecked re-checks it).
//
// Account order (signers first, then writable non-signers, then readonly
// non-signers):
//
//	0 owner          (signer, writable, fee payer)
//	1 source ATA     (writable)
//	2 dest ATA       (writable)
//	3 mint           (readonly)
//	4 token program  (readonly)
//
// The instruction names the token program (index 4) and passes accounts in
// TransferChecked's order: source, mint, dest, owner.
func splTransferMessage(owner, source, dest, mint, tokenProgram, recentBlockhash []byte, amount uint64, decimals uint8) []byte {
	// data: tag 12 (TransferChecked) || u64 LE amount || u8 decimals.
	data := make([]byte, 10)
	data[0] = 12
	binary.LittleEndian.PutUint64(data[1:9], amount)
	data[9] = decimals

	var m []byte
	m = append(m, 1, 0, 2) // header: 1 signer, 0 readonly-signed, 2 readonly-unsigned (mint, token program)
	m = append(m, compactU16(5)...)
	m = append(m, owner...)
	m = append(m, source...)
	m = append(m, dest...)
	m = append(m, mint...)
	m = append(m, tokenProgram...)
	m = append(m, recentBlockhash...)
	m = append(m, compactU16(1)...) // 1 instruction
	m = append(m, 4)                // programIdIndex = token program (account #4)
	m = append(m, compactU16(4)...) // 4 account indices
	m = append(m, 1, 3, 2, 0)       // source, mint, dest, owner (TransferChecked order)
	m = append(m, compactU16(len(data))...)
	m = append(m, data...)
	return m
}

// tokenAccountFor returns the owner's first SPL token account pubkey for mint
// (typically the ATA). ok is false when the owner holds no account for the mint.
func (s *Solana) tokenAccountFor(ctx context.Context, owner, mint string) (string, bool, error) {
	var res struct {
		Value []struct {
			Pubkey string `json:"pubkey"`
		} `json:"value"`
	}
	params := []any{owner, map[string]any{"mint": mint}, map[string]any{"encoding": "jsonParsed"}}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getTokenAccountsByOwner", params, &res); err != nil {
		return "", false, fmt.Errorf("sol: getTokenAccountsByOwner: %w", err)
	}
	if len(res.Value) == 0 {
		return "", false, nil
	}
	return res.Value[0].Pubkey, true, nil
}

// Send signs and broadcasts a Solana devnet transfer: native SOL (asset ""/"SOL")
// or USDC via SPL TransferChecked. amount is a decimal string. Returns the tx
// signature.
func (s *Solana) Send(ctx context.Context, secret []byte, asset, destAddr, amount string) (string, error) {
	switch asset {
	case "", "SOL":
		return s.sendSOL(ctx, secret, destAddr, amount)
	case "USDC":
		return s.sendSPL(ctx, secret, usdcSolanaMint, usdcSolanaDecimals, destAddr, amount)
	default:
		return "", ErrUnsupportedAsset
	}
}

// recentBlockhash fetches a finalized blockhash (32 bytes).
func (s *Solana) recentBlockhash(ctx context.Context) ([]byte, error) {
	var bh struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getLatestBlockhash", []any{map[string]any{"commitment": "finalized"}}, &bh); err != nil {
		return nil, fmt.Errorf("sol: getLatestBlockhash: %w", err)
	}
	blockhash, err := base58Decode(bh.Value.Blockhash)
	if err != nil || len(blockhash) != 32 {
		return nil, fmt.Errorf("sol: bad blockhash")
	}
	return blockhash, nil
}

// submit signs msg with priv and broadcasts the single-signature tx.
func (s *Solana) submit(ctx context.Context, priv ed25519.PrivateKey, msg []byte) (string, error) {
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

func (s *Solana) sendSOL(ctx context.Context, secret []byte, destAddr, amount string) (string, error) {
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
	blockhash, err := s.recentBlockhash(ctx)
	if err != nil {
		return "", err
	}
	return s.submit(ctx, priv, solTransferMessage(from, to, blockhash, lam.Uint64()))
}

// sendSPL moves an SPL token (e.g. USDC) from the signer's token account to the
// recipient's existing token account for the mint. The recipient must already
// hold a token account for the mint (Circle's drip creates one); otherwise this
// returns a clear error rather than creating one (no ATA-create instruction).
func (s *Solana) sendSPL(ctx context.Context, secret []byte, mint string, decimals uint8, destOwnerAddr, amount string) (string, error) {
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("sol: bad seed length %d", len(secret))
	}
	priv := ed25519.NewKeyFromSeed(secret)
	ownerBytes := []byte(priv.Public().(ed25519.PublicKey))
	ownerAddr := base58Encode(ownerBytes)

	if _, err := base58Decode(destOwnerAddr); err != nil {
		return "", fmt.Errorf("sol: bad destination address")
	}
	units, err := baseUnits(amount, int(decimals))
	if err != nil {
		return "", fmt.Errorf("sol: amount: %w", err)
	}
	if !units.IsUint64() {
		return "", fmt.Errorf("sol: amount too large")
	}

	srcTA, ok, err := s.tokenAccountFor(ctx, ownerAddr, mint)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("sol: sender holds no token account for mint %s", mint)
	}
	dstTA, ok, err := s.tokenAccountFor(ctx, destOwnerAddr, mint)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("sol: recipient %s has no token account for mint %s (fund it first)", destOwnerAddr, mint)
	}

	src, err := base58Decode(srcTA)
	if err != nil || len(src) != 32 {
		return "", fmt.Errorf("sol: bad source token account")
	}
	dst, err := base58Decode(dstTA)
	if err != nil || len(dst) != 32 {
		return "", fmt.Errorf("sol: bad dest token account")
	}
	mintBytes, err := base58Decode(mint)
	if err != nil || len(mintBytes) != 32 {
		return "", fmt.Errorf("sol: bad mint")
	}
	tokenProg, err := base58Decode(splTokenProgramID)
	if err != nil || len(tokenProg) != 32 {
		return "", fmt.Errorf("sol: bad token program id")
	}
	blockhash, err := s.recentBlockhash(ctx)
	if err != nil {
		return "", err
	}
	msg := splTransferMessage(ownerBytes, src, dst, mintBytes, tokenProg, blockhash, units.Uint64(), decimals)
	return s.submit(ctx, priv, msg)
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

// Received reports incoming deposits since `cursor` (a tx signature). For each
// newer signature it inspects the tx once, preferring a positive USDC (SPL)
// token-balance delta, else a positive native-SOL delta. Each tx yields exactly
// one Payment so the cursor advances atomically (no per-tx split that a crash
// could half-apply); a send / fee-only tx yields a zero-amount payment that
// only advances the cursor (Credit is a no-op on 0).
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
		sol, usdc, err := s.txDeltas(ctx, sig, address)
		switch {
		case err != nil:
			// Cannot inspect this tx: advance past it (zero-amount no-op credit)
			// rather than wedging the watcher on one bad signature.
			out = append(out, Payment{TxRef: sig, Asset: "SOL", Amount: "0", Cursor: sig})
		case usdc > 0:
			out = append(out, Payment{TxRef: sig, Asset: "USDC", Amount: formatUnits(uint64(usdc), usdcSolanaDecimals), Cursor: sig})
		case sol > 0:
			out = append(out, Payment{TxRef: sig, Asset: "SOL", Amount: formatUnits(uint64(sol), 9), Cursor: sig})
		default:
			out = append(out, Payment{TxRef: sig, Asset: "SOL", Amount: "0", Cursor: sig})
		}
	}
	return out, nil
}

// txDeltas returns the address's native-lamport change and its USDC base-unit
// change (post - pre) in tx sig. The USDC delta sums the owner's matching token
// balances (the ATA may be created in this tx, so a missing pre counts as 0).
func (s *Solana) txDeltas(ctx context.Context, sig, address string) (solDelta, usdcDelta int64, err error) {
	type tokenBal struct {
		Owner      string `json:"owner"`
		Mint       string `json:"mint"`
		UITokenAmt struct {
			Amount string `json:"amount"` // base units, as a string
		} `json:"uiTokenAmount"`
	}
	var tx struct {
		Meta struct {
			PreBalances       []int64    `json:"preBalances"`
			PostBalances      []int64    `json:"postBalances"`
			PreTokenBalances  []tokenBal `json:"preTokenBalances"`
			PostTokenBalances []tokenBal `json:"postTokenBalances"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []string `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	cfg := map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0}
	if err := jsonRPC(ctx, s.hc, s.rpc, "getTransaction", []any{sig, cfg}, &tx); err != nil {
		return 0, 0, err
	}

	// Native SOL delta for the address's own account.
	for i, k := range tx.Transaction.Message.AccountKeys {
		if k == address && i < len(tx.Meta.PreBalances) && i < len(tx.Meta.PostBalances) {
			solDelta = tx.Meta.PostBalances[i] - tx.Meta.PreBalances[i]
			break
		}
	}

	// USDC token delta: sum post minus sum pre over the owner's USDC accounts.
	sumBal := func(bals []tokenBal) int64 {
		var total int64
		for _, b := range bals {
			if b.Owner != address || b.Mint != usdcSolanaMint {
				continue
			}
			if n, ok := new(big.Int).SetString(b.UITokenAmt.Amount, 10); ok && n.IsInt64() {
				total += n.Int64()
			}
		}
		return total
	}
	usdcDelta = sumBal(tx.Meta.PostTokenBalances) - sumBal(tx.Meta.PreTokenBalances)
	return solDelta, usdcDelta, nil
}
