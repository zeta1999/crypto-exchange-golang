# EXTRA-TESTING — Optional manual / network checks

These are **not** part of the automated gate ([TESTING.md](TESTING.md)). They need live
network, external accounts/keys, captcha faucets, or human judgement, so they are run by
hand when relevant — not by CI and not by the per-phase verification subagent. None is
required for a green build.

## Live feed smoke (network: real venues)
```sh
go run ./cmd/feedcat --venue coinbase --symbol BTC-USD   # ~10s
go run ./cmd/feedcat --venue binance  --symbol BTCUSDT   # ~10s
```
Expect: trades stream; best bid < best ask; spread positive. Record + replay:
`--record out.jsonl` then `--replay out.jsonl` reproduces the sequence.

## Live binary as a Coinbase mirror (network)
`EXCHANGE_CONFIG=configs/dev.yaml go run ./cmd/exchange`, then
`curl -sk -H "Authorization: Bearer dev-secret-token" https://localhost:8080/snapshot/BTC-USD`.
Expect a 20-level/side uncrossed book, exact decimal strings.

## CCXT-go conformance (network: pulls the ccxt module)
Boot the binary with the Binance edge on plain HTTP + a seeded `balances:` (see TESTING.md §5
config, add the edge), then:
```sh
cd conformance/ccxt-go && go run .            # binance (HMAC)
```
Expect: `CCXT-GO CONFORMANCE: PASS` (loadMarkets → fetchOrderBook → createLimitOrder →
fetchOpenOrders → cancelOrder). See `conformance/ccxt-go/README.md`.

### Coinbase ES256-JWT conformance (verified)
A **stock `ccxt.NewCoinbase`** (Advanced Trade, ES256 JWT) drives the same lifecycle against the
Coinbase edge — only the base URL is changed. Generate an EC P-256 keypair, boot the edge with
the **public** PEM as `coinbase.jwt_public_key`, and hand the harness the **private** PEM:
```sh
openssl ecparam -genkey -name prime256v1 -noout -out /tmp/cb-priv.pem   # SEC1; PKCS8 also works
openssl ec -in /tmp/cb-priv.pem -pubout -out /tmp/cb-pub.pem
# Boot the exchange with api.coinbase.enabled, jwt_public_key: <contents of cb-pub.pem>,
# products: ["BTC-USD"], balances: { USD, BTC }, emulator.enabled: false, plain HTTP on :8083.
cd conformance/ccxt-go
COINBASE_URL=http://localhost:8083 COINBASE_API_KEY=test-key \
  COINBASE_SECRET_FILE=/tmp/cb-priv.pem go run . coinbase
rm -f /tmp/cb-priv.pem /tmp/cb-pub.pem        # the private key is a secret — delete it
```
Expect `CCXT-GO CONFORMANCE: PASS`. ccxt builds the JWT (`alg:ES256, kid:apiKey`, claims
`sub/exp=nbf+120/uri`) from the PEM secret; the edge verifies the signature against the public
key. Negative check: an unsigned, garbage-bearer, or tampered-signature `POST .../orders` all
return **401** while the ccxt-signed calls succeed. ccxt-go (>= v4.5) discovers via the public
`brokerage/market/products` + `brokerage/market/product_book` paths (aliased to the legacy
routes — see `TestMarketAliasRoutes`).

## Binance `@depth` incremental diff stream (network: localhost)
With the Binance edge enabled, the diff stream emits `depthUpdate` deltas (vs the `@depth20`
partial-book snapshots). Sync like a real client: take the REST snapshot, then apply diffs.
```sh
# REST snapshot id (full book — do NOT use a small limit with @depth):
curl -s "http://localhost:8192/api/v3/depth?symbol=BTCUSDT" | grep -o '"lastUpdateId":[0-9]*'
# Stream diffs (e.g. wscat / websocat):
websocat "ws://localhost:8192/ws/btcusdt@depth"
```
Expect `{"e":"depthUpdate","s":"BTCUSDT","U":..,"u":..,"b":[...],"a":[...]}` with each event's
`U == previous u + 1`, and `u > lastUpdateId` for the first applicable event (removed levels
carry qty `"0"`). Automated coverage: `TestDepthDiffer`, `TestWSDepthDiff`.

## Solana USDC (SPL) on-chain send (network: devnet, faucet-gated)
`custody.Solana.Send(..., "USDC", dest, amount)` builds an SPL `TransferChecked` (resolving the
sender's + recipient's token accounts via RPC) and broadcasts it. The recipient must already
hold a USDC token account (Circle's drip creates one). Live broadcast needs a devnet-funded
USDC hot wallet (Circle key / web faucet — see the custody table). Message layout is
vector-verified offline (`TestSPLTransferMessage`); the resolve + recipient-missing guard is
covered by `TestSendSPL_*`. The Solana deposit *watcher* (`Received`) now credits USDC too: it
inspects each tx's pre/post token balances for the watched owner+mint and emits a USDC payment
(else SOL), so the hub auto-credits the destination ledger — closing the Solana USDC loop in
code (`TestReceived_USDC`/`TestReceived_SOL`). Stellar remains the live-verified full-loop
reference; Solana/EVM/BTC live broadcast is faucet-gated.

## Metrics scrape + rate-limit trip (network: localhost)
With `metrics.enabled: true`: `curl -s localhost:9090/metrics | grep exchange_`. Hammer a
REST edge past `rate_per_sec` and expect `429` / `-1003`.

## Coinbase ES256 JWT (manual: openssl)
Generate an EC P-256 key, set `coinbase.jwt_public_key`, sign a JWT with openssl, and confirm
a valid token → 200, tampered → 401, none → 401.

## Custody live testnet taps (network + sometimes API keys / captcha)
`CUSTODY_PASSPHRASE` set, then per chain:

| Chain | Command | Faucet reality |
|-------|---------|----------------|
| **XLM** | `wallet new -chain xlm`; `faucet`; `balance` | friendbot — reliable, funds ~10000 XLM |
| **SOL** | `wallet new -chain sol`; `faucet`; `balance` | devnet airdrop — heavily rate-limited, often fails; retry |
| **USDC-XLM** | `faucet` (XLM) → `prepare -asset USDC` → `faucet -asset USDC` | trustline is live; drip needs `CIRCLE_API_KEY` (else web faucet URL) |
| **USDC-SOL** | `faucet -asset USDC` | needs `CIRCLE_API_KEY`; else web faucet |
| **ETH / ERC20** | `wallet new -chain eth`; `balance` (Sepolia) | faucet is web/captcha — fund manually, then `balance -watch` |
| **BTC** | `wallet new -chain btc`; `balance` (Esplora) | faucet is web/captcha — fund manually, then `balance -watch` |

Notes: the Circle Stellar blockchain id is a best-guess (`CIRCLE_XLM_BLOCKCHAIN` overrides);
ERC20/USDT on Ethereum is balance-read-only here.

## On-chain transfer flow (network: Stellar testnet)
Move inventory between the two venue accounts on real testnet (Stellar only for now).

1. Create + friendbot-fund two hot wallets in the custody keystore:
   ```sh
   export CUSTODY_PASSPHRASE=xfer CUSTODY_KEYSTORE=/tmp/xfer-ks.json
   custody wallet new -chain xlm -name binance-hot;  custody faucet -name binance-hot
   custody wallet new -chain xlm -name coinbase-hot; custody faucet -name coinbase-hot
   ```
2. Boot the exchange with the `transfer:` block enabled (see configs/dev.yaml), the binance
   edge on, `balances: { XLM: "1000" }`, `transfer.keystore_path: /tmp/xfer-ks.json`, and the
   same `CUSTODY_PASSPHRASE`.
3. Move 100 XLM binance→coinbase via the native endpoint:
   ```sh
   curl -s -X POST localhost:8080/transfer -H 'Content-Type: application/json' \
     -d '{"from":"binance","to":"coinbase","asset":"XLM","amount":"100","token":"<token>"}'
   ```
   (or the Binance-compatible signed `POST /sapi/v1/capital/withdraw/apply` with coin/address/amount).

Expect: a real `tx_ref`; the log line `transfer: credited 100.0000000 XLM to coinbase` after the
deposit poll; and on-chain `custody balance -name binance-hot` ≈ `9899.99999` (sent 100 + fee),
`coinbase-hot` ≈ `10100`. Verified end-to-end on Stellar testnet.

### Other chains / USDC
The transfer hub picks its backend from the venues' wallet chain: **xlm** (Stellar, live-verified),
**eth** (EVM/Sepolia, EIP-155-signing vector-verified), **sol** (Solana/devnet, serialization
vector-verified), **btc** (Bitcoin/testnet, BIP-143-sighash vector-verified). For eth/sol/btc the
hot wallets must hold testnet funds (Sepolia/devnet/testnet faucets — see the custody table above,
mostly captcha/key-gated), then a `/transfer` or signed withdraw moves them on-chain the same way.
**USDC** transfers work once a hot wallet holds USDC: on Stellar establish the trustline
(`custody prepare`) + fund via Circle (`CIRCLE_API_KEY`), then `/transfer ... asset=USDC` (the
Stellar `Send` routes USDC as a CreditAsset); on EVM, USDC/USDT move as ERC20 `transfer` calldata.

### Coinbase CCXT conformance
Done — see "Coinbase ES256-JWT conformance (verified)" above. A stock `ccxt.NewCoinbase`
(Advanced Trade) runs the full signed lifecycle against the edge with a matching EC key.
