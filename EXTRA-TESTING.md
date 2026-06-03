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
