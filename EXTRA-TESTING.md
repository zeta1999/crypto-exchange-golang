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
cd conformance/ccxt-go && go run .
```
Expect: `CCXT-GO CONFORMANCE: PASS` (loadMarkets → fetchOrderBook → createLimitOrder →
fetchOpenOrders → cancelOrder). See `conformance/ccxt-go/README.md`.

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
The Coinbase edge's public market-data endpoints (`/products`, `/product_book`) are
CCXT-`loadMarkets`/`fetchOrderBook` compatible (same shape as the Binance harness in
`conformance/ccxt-go`). Signed calls (createOrder/cancel) use **ES256 JWT** — configure
`coinbase.jwt_public_key` and point `ccxt.NewCoinbase` (the Advanced Trade class) at the edge
with a matching EC key. A full signed run is the live follow-up.
