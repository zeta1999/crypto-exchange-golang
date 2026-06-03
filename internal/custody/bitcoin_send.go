package custody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// --- low-level helpers ---

func dsha256(b []byte) []byte {
	h1 := sha256.Sum256(b)
	h2 := sha256.Sum256(h1[:])
	return h2[:]
}

func le32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

func le64(w *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.Write(b[:])
}

// varInt is Bitcoin's compact size encoding.
func varInt(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		b := []byte{0xfd, 0, 0}
		binary.LittleEndian.PutUint16(b[1:], uint16(n))
		return b
	case n <= 0xffffffff:
		b := []byte{0xfe, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(b[1:], uint32(n))
		return b
	default:
		b := []byte{0xff, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.LittleEndian.PutUint64(b[1:], n)
		return b
	}
}

func writeVarBytes(w *bytes.Buffer, b []byte) {
	w.Write(varInt(uint64(len(b))))
	w.Write(b)
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// btcIn / btcOut model a tx for sighash + serialization.
type btcIn struct {
	txidLE   []byte // 32, little-endian (reversed display txid)
	vout     uint32
	sequence uint32
}

type btcOut struct {
	value  uint64
	script []byte // scriptPubKey
}

// bip143Sighash computes the BIP-143 segwit sighash (SIGHASH_ALL) for inIdx.
// scriptCode for P2WPKH is 0x76a914{hash160}88ac (without its length prefix).
func bip143Sighash(version uint32, ins []btcIn, outs []btcOut, inIdx int, scriptCode []byte, amount uint64, locktime uint32) []byte {
	var prevouts, seqs, outputs bytes.Buffer
	for _, in := range ins {
		prevouts.Write(in.txidLE)
		le32(&prevouts, in.vout)
		le32(&seqs, in.sequence)
	}
	for _, o := range outs {
		le64(&outputs, o.value)
		writeVarBytes(&outputs, o.script)
	}
	var p bytes.Buffer
	le32(&p, version)
	p.Write(dsha256(prevouts.Bytes()))
	p.Write(dsha256(seqs.Bytes()))
	p.Write(ins[inIdx].txidLE)
	le32(&p, ins[inIdx].vout)
	writeVarBytes(&p, scriptCode)
	le64(&p, amount)
	le32(&p, ins[inIdx].sequence)
	p.Write(dsha256(outputs.Bytes()))
	le32(&p, locktime)
	le32(&p, 1) // SIGHASH_ALL
	return dsha256(p.Bytes())
}

// p2wpkhScriptCode returns 0x76a914{hash160}88ac for a 20-byte key hash.
func p2wpkhScriptCode(h160 []byte) []byte {
	s := []byte{0x76, 0xa9, 0x14}
	s = append(s, h160...)
	return append(s, 0x88, 0xac)
}

// p2wpkhScriptPubKey returns OP_0 <20-byte program> (0x0014{h160}).
func p2wpkhScriptPubKey(program []byte) []byte {
	return append([]byte{0x00, 0x14}, program...)
}

// --- bech32 decode (BIP-173) ---

func bech32Decode(s string) (hrp string, data []int, err error) {
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, fmt.Errorf("bech32: bad separator")
	}
	hrp = s[:pos]
	var raw []int
	for _, c := range s[pos+1:] {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return "", nil, fmt.Errorf("bech32: bad char %q", c)
		}
		raw = append(raw, idx)
	}
	if bech32Polymod(append(bech32HrpExpand(hrp), raw...)) != 1 {
		return "", nil, fmt.Errorf("bech32: bad checksum")
	}
	return hrp, raw[:len(raw)-6], nil
}

// segwitProgram decodes a bech32 segwit address to its witness program bytes.
func segwitProgram(hrp, addr string) ([]byte, error) {
	h, data, err := bech32Decode(addr)
	if err != nil {
		return nil, err
	}
	if h != hrp || len(data) < 1 {
		return nil, fmt.Errorf("btc: wrong network/empty address")
	}
	conv, err := convertBits5to8(data[1:])
	if err != nil {
		return nil, err
	}
	return conv, nil
}

// convertBits5to8 regroups 5-bit groups back to bytes (no padding allowed).
func convertBits5to8(data []int) ([]byte, error) {
	acc, bits := 0, uint(0)
	var out []byte
	for _, v := range data {
		acc = (acc << 5) | v
		bits += 5
		for bits >= 8 {
			bits -= 8
			out = append(out, byte((acc>>bits)&0xff))
		}
	}
	if bits >= 5 || ((acc<<(8-bits))&0xff) != 0 {
		return nil, fmt.Errorf("bech32: bad padding")
	}
	return out, nil
}

// --- Esplora UTXO / broadcast ---

type esploraUTXO struct {
	TxID  string `json:"txid"`
	Vout  uint32 `json:"vout"`
	Value uint64 `json:"value"`
}

func (b *Bitcoin) utxos(ctx context.Context, address string) ([]esploraUTXO, error) {
	body, err := httpGet(ctx, b.hc, b.esplora+"/address/"+address+"/utxo")
	if err != nil {
		return nil, fmt.Errorf("btc: utxos: %w", err)
	}
	var u []esploraUTXO
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("btc: decode utxos: %w", err)
	}
	return u, nil
}

// Send signs and broadcasts a testnet P2WPKH transfer of `amount` BTC to a tb1
// address. Selects UTXOs, pays a flat fee estimate, returns change to the
// sender, and broadcasts via Esplora. Returns the txid.
func (b *Bitcoin) Send(ctx context.Context, secret []byte, asset, destAddr, amount string) (string, error) {
	if asset != "" && asset != "BTC" {
		return "", ErrUnsupportedAsset
	}
	if len(secret) != 32 {
		return "", fmt.Errorf("btc: bad key length %d", len(secret))
	}
	priv := secp256k1.PrivKeyFromBytes(secret)
	pub := priv.PubKey().SerializeCompressed()
	fromH160 := hash160(pub)
	fromAddr, err := b.Address(secret)
	if err != nil {
		return "", err
	}
	destProgram, err := segwitProgram(btcHRP, destAddr)
	if err != nil {
		return "", err
	}
	sendSats, err := baseUnits(amount, 8)
	if err != nil {
		return "", fmt.Errorf("btc: amount: %w", err)
	}
	if !sendSats.IsUint64() {
		return "", fmt.Errorf("btc: amount too large")
	}
	send := sendSats.Uint64()

	utxos, err := b.utxos(ctx, fromAddr)
	if err != nil {
		return "", err
	}
	// Select UTXOs (largest-first) to cover send + a flat fee. P2WPKH inputs are
	// ~68 vbytes; a flat fee is plenty on testnet.
	const flatFee = uint64(500)
	var ins []btcIn
	var inVals []uint64
	var total uint64
	for _, u := range utxos {
		ins = append(ins, btcIn{txidLE: reverse(mustHex(u.TxID)), vout: u.Vout, sequence: 0xffffffff})
		inVals = append(inVals, u.Value)
		total += u.Value
		if total >= send+flatFee {
			break
		}
	}
	if total < send+flatFee {
		return "", fmt.Errorf("btc: insufficient confirmed UTXOs (%d sat) for %d + fee", total, send)
	}
	outs := []btcOut{{value: send, script: p2wpkhScriptPubKey(destProgram)}}
	if change := total - send - flatFee; change > 546 { // above dust
		outs = append(outs, btcOut{value: change, script: p2wpkhScriptPubKey(fromH160)})
	}

	// Sign each input (BIP-143) and assemble witnesses.
	scriptCode := p2wpkhScriptCode(fromH160)
	witnesses := make([][][]byte, len(ins))
	for i := range ins {
		h := bip143Sighash(2, ins, outs, i, scriptCode, inVals[i], 0)
		sig := ecdsa.Sign(priv, h)
		der := append(sig.Serialize(), 0x01) // DER + SIGHASH_ALL
		witnesses[i] = [][]byte{der, pub}
	}
	raw := serializeSegwitTx(2, ins, outs, witnesses, 0)

	body, err := httpPost(ctx, b.hc, b.esplora+"/tx", hex.EncodeToString(raw))
	if err != nil {
		return "", fmt.Errorf("btc: broadcast: %w", err)
	}
	return strings.TrimSpace(string(body)), nil // Esplora returns the txid
}

// serializeSegwitTx serializes a signed segwit (BIP-144) transaction.
func serializeSegwitTx(version uint32, ins []btcIn, outs []btcOut, witnesses [][][]byte, locktime uint32) []byte {
	var w bytes.Buffer
	le32(&w, version)
	w.Write([]byte{0x00, 0x01}) // segwit marker + flag
	w.Write(varInt(uint64(len(ins))))
	for _, in := range ins {
		w.Write(in.txidLE)
		le32(&w, in.vout)
		w.Write(varInt(0)) // empty scriptSig (segwit)
		le32(&w, in.sequence)
	}
	w.Write(varInt(uint64(len(outs))))
	for _, o := range outs {
		le64(&w, o.value)
		writeVarBytes(&w, o.script)
	}
	for _, wit := range witnesses {
		w.Write(varInt(uint64(len(wit))))
		for _, item := range wit {
			writeVarBytes(&w, item)
		}
	}
	le32(&w, locktime)
	return w.Bytes()
}

// LatestCursor returns the address's most recent txid (Esplora), so a fresh
// watcher only sees newer txs.
func (b *Bitcoin) LatestCursor(ctx context.Context, address string) (string, error) {
	txs, err := b.addressTxs(ctx, address, "")
	if err != nil {
		return "", err
	}
	if len(txs) == 0 {
		return "", nil
	}
	return txs[0].TxID, nil // newest-first
}

type esploraTx struct {
	TxID string `json:"txid"`
	Vout []struct {
		Value               uint64 `json:"value"`
		ScriptPubKeyAddress string `json:"scriptpubkey_address"`
	} `json:"vout"`
}

func (b *Bitcoin) addressTxs(ctx context.Context, address, afterTxID string) ([]esploraTx, error) {
	url := b.esplora + "/address/" + address + "/txs"
	if afterTxID != "" {
		url += "/chain/" + afterTxID
	}
	body, err := httpGet(ctx, b.hc, url)
	if err != nil {
		return nil, fmt.Errorf("btc: address txs: %w", err)
	}
	var txs []esploraTx
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, fmt.Errorf("btc: decode txs: %w", err)
	}
	return txs, nil
}

// Received reports incoming BTC since `cursor` (a txid): the sum of outputs paying
// `address` in each newer tx.
func (b *Bitcoin) Received(ctx context.Context, address, cursor string) ([]Payment, error) {
	txs, err := b.addressTxs(ctx, address, cursor)
	if err != nil {
		return nil, err
	}
	var out []Payment
	for i := len(txs) - 1; i >= 0; i-- { // oldest-first so the cursor moves forward
		tx := txs[i]
		var sats uint64
		for _, vo := range tx.Vout {
			if vo.ScriptPubKeyAddress == address {
				sats += vo.Value
			}
		}
		amt := "0"
		if sats > 0 {
			amt = formatUnits(sats, 8)
		}
		out = append(out, Payment{TxRef: tx.TxID, Asset: "BTC", Amount: amt, Cursor: tx.TxID})
	}
	return out, nil
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }
