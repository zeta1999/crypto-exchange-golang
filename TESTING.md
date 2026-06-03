# TESTING — Automated test plan

Every step here is **fully automated and offline** (no network, no manual judgement): a
human or a subagent can run it top-to-bottom and each step either passes or fails on its
own. It is the per-phase verification gate. Manual / network-dependent / not-easily-automatable
checks (live feeds, live testnet faucets, live external-client conformance, metrics scrape)
live in **[EXTRA-TESTING.md](EXTRA-TESTING.md)** and are optional.

Run from the repo root. The single command that must pass is `./ci.sh`; the targeted
sections below let a reviewer confirm a specific subsystem in isolation.

## 0. Full gate (the one that matters)

```sh
./ci.sh
```
**Expected:** `CI PASSED`, exit 0 — gofmt clean, `go vet` clean, `go build ./...` ok,
`go test ./... -race -count=1` all green. (Nested modules under `conformance/` are
intentionally excluded.)

## 1. Matching core + exact decimals

```sh
go test ./internal/orderbook/... ./internal/engine/... ./pkg/decimal/... -count=1
```
**Expected:** green. Decimal arithmetic is validated against a `math/big.Rat` oracle and is
allocation-free.

## 2. Emulator: determinism, replay, fault injection, arb, latency

```sh
go test ./internal/emulator/... ./internal/feed/replay/... ./internal/toxicity/... -count=1
```
**Expected:** green. Covers the deterministic-clock golden test (byte-reproducible runs),
replay speed pacing, RTR convergence, toxicity, the cross-venue `ArbHarness`
(exploitable-then-closeable), and latency injection — uniform **and** shifted-Poisson
(including the large-λ no-hang guard).

## 3. API edges (Binance + Coinbase) incl. account balance ledger

```sh
go test ./internal/api/binance/... ./internal/api/coinbase/... ./internal/account/... \
        ./internal/api/httpserver/... ./internal/transfer/... -count=1
```
**Expected:** green. Covers signed REST/WS, the CCXT-style body-signed POST, exchangeInfo /
products discovery, the **CR-5 OMS-parity surface** (place-time idempotency on
`newClientOrderId` → duplicate place rejected -2010, never a 2nd resting order, incl. the
concurrent-race test; **GET /api/v3/order** signed query by orderId/origClientOrderId across
NEW→CANCELED; and the end-to-end acceptance flow place→GET→openOrders→duplicate-noop→cancel),
fill-report latency, the **Binance `@depth` incremental diff stream**
(depthUpdate deltas + monotonic U/u continuity, REST `lastUpdateId` sync), the **Coinbase fee
fields** (total_fees/total_value_after_fees per side, product price_increment), the **Coinbase
level2 incremental diffs** (snapshot-then-changed-levels, removals as new_quantity "0",
snapshot-first ordering), the **balance
ledger** (lock-on-place, settle-on-fill with price-improvement refund, cancel-unlock,
insufficient-balance rejection, underfunded-market no-mint guard), and the **transfer flow**
(withdraw endpoint, native `/transfer`, hub debit→send→credit with the precision-quantize
guard, fake-backend deposit watcher).

## 4. Custody toolkit (encoders, keystore, chains)

```sh
go test ./internal/custody/... -count=1
```
**Expected:** green. StrKey/base58/bech32/EIP-55 validated against **real on-chain vectors**
(Circle USDC issuer/mint, secp256k1 privkey=1, BIP-173); transaction signing vector-checked
(EIP-155, BIP-143, Solana System transfer **and SPL `TransferChecked` for USDC**); the Solana
SPL send's token-account resolution + recipient-missing-account guard, and SPL/native deposit
detection in the watcher (USDC token-balance delta vs SOL lamport delta — httptest RPC fake);
Argon2id+memguard keystore round-trip + wrong-passphrase + downgraded-KDF rejection;
deposit-watch detection for **EVM USDC** (ERC20 Transfer logs) + **BTC** (Esplora address
vouts) + **Solana** USDC/SOL (httptest RPC/Esplora fakes); Circle/SPL/faucet guards (no network).

## 5. Offline binary smoke (replay venue, plain HTTP, no network)

A scriptable end-to-end boot with the emulator fed from the recorded trace and a balance
ledger seeded. Writes a temp config, boots, curls, and tears down.

```sh
cat > /tmp/test-smoke.yaml <<'YAML'
network: { listen_grpc: ":50091", listen_http: ":8190", listen_ws: ":8191", token_secret: "t", tls: { cert_file: "", key_file: "" } }
database: { dsn: "memory" }
limits: { max_open_orders: 5000, min_tick_size: 0.01 }
instruments: [ { symbol: "BTC-USD", base: "BTC", quote: "USD" } ]
storage: { wal_path: "data/test-smoke.wal" }
api:
  binance:
    enabled: true
    listen: ":8192"
    api_key: "k"
    secret: "s"
    symbols: [ { binance: "BTCUSDT", engine: "BTC-USD" } ]
    balances: { USD: "1000000", BTC: "10" }
metrics: { enabled: false }
emulator:
  enabled: true
  venue: "replay"
  instruments: ["BTC-USD"]
  reference: { depth_levels: 20, refresh_ms: 250 }
  rtr: { tau_ms: 0 }
  replay: { file: "testdata/feed/sample.jsonl", speed: 0.0 }
YAML
# Build a real binary and run THAT (not `go run`), so $PID is the server itself
# and the kill below actually reaps it — `kill` on a `go run` wrapper leaves the
# compiled child holding the ports for the next run.
go build -o /tmp/test-exchange ./cmd/exchange
EXCHANGE_CONFIG=/tmp/test-smoke.yaml /tmp/test-exchange >/tmp/test-smoke.log 2>&1 &
PID=$!; sleep 8
grep -q "address already in use" /tmp/test-smoke.log && echo "FAIL boot (port in use — kill stale exchange procs)" || true
curl -s "http://localhost:8192/api/v3/exchangeInfo" | grep -q '"symbol":"BTCUSDT"' && echo "OK exchangeInfo" || echo "FAIL exchangeInfo"
curl -s "http://localhost:8192/api/v3/depth?symbol=BTCUSDT&limit=2" | grep -q '"bids"' && echo "OK depth" || echo "FAIL depth"
kill $PID 2>/dev/null; rm -f /tmp/test-smoke.yaml /tmp/test-exchange data/test-smoke.wal
```
**Expected:** the binary boots (emulator mirrors the replay trace), and both curls print
`OK …`. (The book is seeded from `testdata/feed/sample.jsonl`; no live venue is contacted.)
If you see `FAIL boot`, a previous run's process is still holding the ports — kill it
(`pkill -f test-exchange` / `lsof -ti tcp:8192 | xargs kill`) and re-run.

## 6. Custody CLI smoke (wallet create + encrypted keystore, no network)

```sh
export CUSTODY_PASSPHRASE="testing"; export CUSTODY_KEYSTORE="/tmp/test-custody.json"; rm -f "$CUSTODY_KEYSTORE"
go run ./cmd/custody wallet new -chain xlm -name a >/dev/null && \
go run ./cmd/custody wallet new -chain eth -name b >/dev/null && \
go run ./cmd/custody wallet list && \
grep -q '"kdf": "argon2id"' "$CUSTODY_KEYSTORE" && echo "OK keystore argon2id" || echo "FAIL keystore"
rm -f "$CUSTODY_KEYSTORE"; unset CUSTODY_PASSPHRASE CUSTODY_KEYSTORE
```
**Expected:** two wallets listed (`xlm` G… and `eth` 0x…); keystore declares `argon2id`.
Balance/faucet against live testnets is in EXTRA-TESTING.md.

## 7. OMS-test preset boot smoke (CR-5, plain HTTP, no network)

Boots the consumer-facing preset `configs/oms-test.yaml` **verbatim** (Binance edge on plain
HTTP :8192, `emulator.enabled:false`, seeded balances, BTCUSDT↔BTC-USD) and drives the OMS
acceptance flow with a signed client: place a LIMIT (echoed clientOrderId) → GET
/api/v3/order by origClientOrderId → openOrders lists it → a duplicate place is rejected (no
2nd order) → cancel by origClientOrderId.

```sh
go build -o /tmp/oms-exchange ./cmd/exchange
EXCHANGE_CONFIG=configs/oms-test.yaml /tmp/oms-exchange >/tmp/oms-smoke.log 2>&1 &
PID=$!; sleep 6
sig() { printf '%s' "$1" | openssl dgst -sha256 -hmac "s" | sed 's/^.* //'; }
B=http://localhost:8192; COID=smoke-1
# place
Q="symbol=BTCUSDT&side=BUY&type=LIMIT&timeInForce=GTC&quantity=0.01&price=100&newClientOrderId=$COID&timestamp=1700000000000"
curl -s -H "X-MBX-APIKEY: k" -X POST "$B/api/v3/order?$Q&signature=$(sig "$Q")" | grep -q "\"clientOrderId\":\"$COID\"" && echo "OK place" || echo "FAIL place"
# query by origClientOrderId
Q="symbol=BTCUSDT&origClientOrderId=$COID&timestamp=1700000000000"
curl -s -H "X-MBX-APIKEY: k" "$B/api/v3/order?$Q&signature=$(sig "$Q")" | grep -q '"status":"NEW"' && echo "OK query" || echo "FAIL query"
# openOrders
Q="symbol=BTCUSDT&timestamp=1700000000000"
curl -s -H "X-MBX-APIKEY: k" "$B/api/v3/openOrders?$Q&signature=$(sig "$Q")" | grep -q "$COID" && echo "OK openOrders" || echo "FAIL openOrders"
# duplicate place rejected
Q="symbol=BTCUSDT&side=BUY&type=LIMIT&timeInForce=GTC&quantity=0.01&price=100&newClientOrderId=$COID&timestamp=1700000000000"
curl -s -H "X-MBX-APIKEY: k" -X POST "$B/api/v3/order?$Q&signature=$(sig "$Q")" | grep -q '"code":-2010' && echo "OK duplicate-rejected" || echo "FAIL duplicate"
# cancel by origClientOrderId
Q="symbol=BTCUSDT&origClientOrderId=$COID&timestamp=1700000000000"
curl -s -H "X-MBX-APIKEY: k" -X DELETE "$B/api/v3/order?$Q&signature=$(sig "$Q")" | grep -q '"status":"CANCELED"' && echo "OK cancel" || echo "FAIL cancel"
kill $PID 2>/dev/null; rm -f /tmp/oms-exchange data/oms-test.wal
```
**Expected:** every line prints `OK …`. This is the same scenario asserted by the Go tests in
§3 (`TestOMSFlow_Acceptance`, `TestPlaceOrder_Duplicate*`, `TestQueryOrder_*`); the boot smoke
additionally proves the shipped preset is bootable verbatim on plain HTTP.

---

**Per-phase rule:** after a phase → `./ci.sh` → brutal-review subagent + fixes → a subagent
runs THIS file end-to-end → iterate until CI passes and every step here reports OK.
