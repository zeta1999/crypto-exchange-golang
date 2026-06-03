# TODO

Granular checklist. Keep PRs/commits scoped to one phase. CI (`./ci.sh`) must be green
before each commit. **Commit but do not push.** After each phase: brutal review subagent →
fix → CI → manual TESTING subagent → iterate until clean.

## Phase 0 — Foundations, CI, docs
- [x] Write PLAN.md, STATUS.md, TODO.md, TESTING.md
- [x] Add `ci.sh` (gofmt check, go vet, golangci-lint if present, build, test -race)
- [x] Add `Makefile` (`ci`, `run`, `test`, `lint`, `fmt`)
- [x] Run baseline `./ci.sh`; record result in STATUS — **green** 2026-06-02
- [x] Commit Phase 0 (no push)
- [x] Brutal review subagent + fixes (gofmt grep/pipefail bug, test -timeout, make lint mask, feedcat stub)

## Phase 1 — Feed ingestion layer
- [x] `internal/feed/types.go`: `Trade`, `LOBSnapshot`, `LOBLevel`, `Ticker`, `Event`
- [x] `internal/feed/source.go`: `Source` interface (`Start/Name/Status`) + `StatusTracker`
- [x] `internal/feed/binance/`: port `@depth20@100ms` + `@trade` (channel-based)
- [x] `internal/feed/coinbase/`: port `market_trades`; implement `level2` (l2_data) parse+emit
- [x] `internal/feed/replay/`: file-backed source (deterministic) + `Recorder`
- [x] feed record mode (`feedcat -record`) → JSONL; sample under `testdata/feed/`
- [x] `cmd/feedcat/`: print live normalized trades+book (live-verified both venues)
- [x] unit tests (parse fixtures → expected events; record/replay round-trip)
- [x] brutal review subagent + fixes (recv-time determinism, parse-failure drops, read deadline + ctx-close, RunReconnect backoff, error counting, lifecycle test)

## Phase 2 — Reference book
- [x] `internal/reference/book.go`: per-instrument LOB, snapshot+diff apply (float-keyed)
- [x] sequence-gap detection (connection-global, in Coinbase adapter), staleness + crossed-book flags
- [x] `BestBidAsk`, `Depth(n)`, `Mid`, `Spread`, `Crossed`, immutable `Snapshot`
- [x] `internal/reference/set.go`: per-instrument routing + `Consume`
- [x] tests vs recorded feed fixtures (deterministic book state) + live verify
- [x] brutal review subagent + fixes (seq-design C1, key-collapse H2, crossed-book H3)

## Phase 3 — Emulator seeding
- [x] `internal/emulator/seeder.go`: reference levels → synthetic resting orders (tagged)
- [x] reconcile loop (add/cancel/resize to match reference; cancel-before-place; not-found-tolerant)
- [x] config: instrument, depth levels; `Run(interval)` cadence + `Clear`
- [x] tests: no user activity ⇒ engine book == reference (+ resize/cancel/cap/idempotent/skip)
- [x] brutal review + fixes (ErrOrderNotFound benign; proved no cross-side self-match)
- [ ] (deferred to Phase 4) partial-fill top-up accounting; venue/refresh wiring into configs/cmd

## Phase 4 — Return-to-Reference [a]
- [x] fill accounting: top partially-eaten levels back up; generation-stamped IDs; volEps tolerance compare
- [x] `internal/emulator/rtr.go`: stale synthetics drain toward zero (decay)
- [x] progressive convergence over `tau` (exp decay: α=1−e^(−dt/τ))
- [x] track spot moves (new levels ramp in, departed levels drain — same Converge path)
- [x] scenario test: perturb → converge (seeded, deterministic) + fill-accounting soundness
- [x] brutal review + fixes (generation IDs, single fill path, RTR.Run dt)
- [ ] (still open) seeder lock held across engine calls — acceptable now; revisit if Clear/shutdown latency matters at scale

## Phase D — Fixed-point decimals (foundational; prices & quantities) — see PLAN.md §9
- [x] `pkg/decimal`: `Decimal` (base-10, 18 frac digits, signed 128-bit scaled storage `{hi,lo}`)
- [x] construction: `FromRaw`/`Raw`, `FromInt`, `FromFloat` (lossy, non-finite panics), `Parse`/`MustParse`
- [x] format: `Float64`, `String`/`StringPrec` (truncate), robust JSON/Text marshaling as decimal string
- [x] arithmetic: `Add`/`Sub` (math/bits carry + overflow detect), `Mul`/`Div` (big.Int interim; div-by-zero/overflow panic), `Neg`/`Abs`/`Sign`/`IsZero`
- [x] compare: `Cmp`/`Eq`/`Lt`/`Lte`/`Gt`/`Gte`, free `Min`/`Max`/`Abs`; comparable map-key (proven canonical)
- [x] tests: Parse↔String round-trip; arithmetic vs math/big.Rat oracle (5k); overflow/edge; JSON robustness
- [x] brutal review + fixes (JSON quote-strip, FromFloat non-finite, perf TODOs)
- [x] (perf) Mul → allocation-free 128-bit limb math (PLAN §9.7); oracle-validated, 0 allocs/op + benchmark
- [x] (perf) Div → allocation-free 256÷128 binary long division; oracle-validated, 0 allocs/op + boundary tests (PLAN §9.7). pkg/decimal now fully allocation-free.
- [ ] (optional) float64-backed backend behind same API for A/B + fast mode
- [x] migration (PLAN.md §9.8): matching core (orderbook/engine/margin) → reference (parses feed decimal strings) → emulator → API edges convert at boundary; feed stays float64. CI green; live-verified (HTTP emits exact decimal strings). Follow-up: exact alpha=1 snap + 1e-9 convergence tolerance.

## Phase 5 — Trade replay sync
- [x] `internal/emulator/replay.go`: tape trade → IOC marketable order vs engine book (single-lock, no resting remainder)
- [x] fill user limits in sync with tape price (user fills at own price by priority; synthetic absorbs rest → RTR refills)
- [x] wired into binary: feed subscribes trades; per-instrument tape goroutines; tape orders margin-exempt
- [x] tests: user limit fills consistent with tape (buy/sell), price-capped, IOC no-remainder, non-finite/unknown-side guards
- [x] brutal review + fixes (IOC primitive vs place-then-cancel, NaN/Inf panic guard, HOL decouple)
- [x] real-time vs accelerated (`speed`) clock — done in Phase 7 (`replay.WithSpeed`)

## Phase 6 — Configurable toxicity [b] — DONE
- [x] `internal/toxicity/kyle.go`: signed-volume → Δprice regression (λ)
- [x] `internal/toxicity/vpin.go`: volume buckets, informed-trade proxy (single-print capped)
- [x] `internal/emulator/toxic.go`: adverse-selection injector (seeded; scale·Score prob; scale·Impact penetration bounded to 1 spread)
- [x] config knobs (`scale`/`kyle_weight`/`vpin_weight`/`window_trades`/`bucket_volume`/`buckets`/`seed`) + wired into binary
- [x] tests: high toxicity ⇒ picks off resting user order; `scale:0` ⇒ pure RTR; non-finite guard
- [x] brutal review + fixes (bounded sweep, panic guard, weight/VPIN clamps)

## Phase 7 — Scenario & fault injection (OMS / strategy test bed)
- [x] Trace replay (full): `venue: replay` feeds the whole emulator from a recorded trace, offline + deterministic (reuses Phase 1 replay.Source; integration-tested + live-verified). **`speed` pacing done** (`replay.WithSpeed`: <=0 fast/deterministic, 1.0 real time, Nx).
- [x] `internal/emulator/latency.go`: artificial latency — feed→book (wired), **order_ack + fill_report wired at both API edges** (WithAckDelay sync; WithFillDelay async-holds the fill WS push; live-verified), per-edge. Jitter is **uniform OR shifted-Poisson** (`distribution` knob).
- [x] `internal/emulator/priceshift.go`: artificial price shift — `offset_bps` + `scale` per venue (wired into dispatcher; shifts both float Price and PriceDecimal)
- [x] cross-venue dislocation harness (`ArbHarness` mirrors one reference into two shifted venues; `CrossArb` detector; test proves exploitable-then-closeable arb)
- [x] scenario scripting format (JSONL): timeline of injection events (`scenario.go`; runtime-mutable `Controls`; price_shift+latency actions; deterministic; reviewed)
- [x] config: `emulator.{latency,price_shift,scenario}`
- [x] deterministic clock (`OrderBook.SetClock`) + golden tests: zeroed controls = no-op; injected latency shows in ack/fill timestamps; seeded scenario reproduces bit-for-bit; arb scenario is exploitable then closes

## Protocol compliance (cross-cutting, Phases 8–9)
- [x] Validated the Binance edge against the **stock `ccxt-go` v4 client** with **only the
      base URL changed** (`conformance/ccxt-go/`): loadMarkets → fetchOrderBook →
      createLimitOrder → fetchOpenOrders → cancelOrder all pass. Surfaced + fixed two real
      conformance bugs (sign over query+body; timestamp from body).
- [x] Unmodified client (endpoint swap only); kept as a separate nested module so its dep tree
      stays out of the main module + CI.
- [x] Conformance drive: place/cancel/query + read order book via the stock client.
- [x] Coinbase conformance via CCXT — stock `ccxt.NewCoinbase` (Advanced Trade, ES256 JWT),
      `go run . coinbase`. Surfaced + fixed the `brokerage/market/*` public path migration
      (market/products + market/product_book aliases). Verified PASS; 401 on unsigned/tampered.

## Phase 8 — Binance-compatible API
- [x] `internal/api/binance/rest.go`: order POST/DELETE, openOrders, depth, ticker, account (stub balances)
- [x] HMAC-SHA256 signature emulation + timestamp/recvWindow (constant-time; -1022/-1021/-2014/-2015)
- [x] symbol mapping (config BTCUSDT↔BTC-USD); registry w/ hook-driven fill tracking; wired behind config
- [x] tests (23) + brutal review + fixes (panic guard, phantom-record rollback); live-verified signed order
- [x] `internal/api/binance/ws.go`: market streams (@trade/@depth20 + **@depth incremental diffs**: depthUpdate U/u, REST lastUpdateId sync) + user-data executionReport (listenKey)
- [x] /exchangeInfo (CCXT loadMarkets: symbols + PRICE_FILTER/LOT_SIZE/NOTIONAL); real balances still deferred
- [x] latency injection applied at this edge (order_ack sync + fill_report async)
- [x] conformance: ccxt-go v4 (endpoint-swapped) — PASS

## Phase 9 — Coinbase-compatible API
- [x] `internal/api/coinbase/rest.go`: create/batch_cancel/list orders (historical), product_book, products/ticker, accounts (stub)
- [x] HMAC auth emulation (CB-ACCESS-*, base64-or-raw secret, ±30s window); JWT/ES256 deferred
- [x] product allow-list; registry w/ hook fill tracking; record-before-place + rollback; wired behind config (:8083)
- [x] tests (31) + brutal review (clean, no fixes needed); live-verified signed create + list
- [x] `internal/api/coinbase/ws.go`: level2, market_trades, user channels (message-based subscribe)
- [x] JWT/ES256 production auth (`jwt.go`: ECDSA P-256 verify, Bearer REST + WS jwt field; dependency-free; live-verified)
- [x] /products list endpoint (CCXT loadMarkets: base/quote ids + increments + min size)
- [x] latency injection applied at this edge (order_ack sync + fill_report async)
- [x] fee/precision fields — orderView + WS userOrder `total_fees`/`total_value_after_fees`
      (configurable `fee_rate`, default 0.6%), product `price_increment`; persisted terminal-order history (done earlier)
- [x] conformance via CCXT (endpoint-swapped) — stock `ccxt.NewCoinbase` ES256-JWT lifecycle PASS
      (`conformance/ccxt-go` coinbase mode); needed `brokerage/market/*` public-path aliases

## Phase 10 — Custody examples (stretch, testnet only) — DONE (toolkit)
- [x] `internal/custody/chain.go`: `Chain`/`Faucet`/`TokenPreparer` interfaces + testnet-only `MustTestnet` guard
- [x] **Encrypted keystore**: AES-256-GCM, **Argon2id** KDF (single-lane, 64 MiB; memguard locked-memory enclave for the key), `CUSTODY_PASSPHRASE` env-only
- [x] XLM (friendbot, Horizon) + USDC trustline (stellar/go changeTrust sign+submit)
- [x] Solana (devnet airdrop, getBalance) + USDC SPL balance
- [x] ETH/ERC20 on Sepolia (secp256k1/keccak EIP-55, eth_getBalance + balanceOf)
- [x] Bitcoin testnet (secp256k1, hand-rolled bech32 P2WPKH `tb1`, Esplora balance)
- [x] USDC faucet via Circle drip (`CIRCLE_API_KEY`) with web-faucet fallback; `cmd/custody` CLI; off by default, NOT wired into the server. Encoders validated against real on-chain vectors + live taps.
- [x] account balance ledger → live /account + /accounts (lock/settle on trade)
- [x] **on-chain transfer flow** (`internal/transfer`): withdraw debits a venue ledger → real
      testnet payment from its custody hot wallet → deposit watcher credits the destination
      ledger. Binance `/sapi/.../withdraw/apply` + native `/transfer`. Live-verified on Stellar
      testnet (XLM). Custody gained an on-chain `Sender` (Stellar Payment + deposit detection).
- [x] on-chain sends for **EVM/Solana/Bitcoin** (per-chain tx builders, each verified against a
      canonical vector: EIP-155, Solana message serialization, BIP-143); the transfer hub picks
      its backend by the venues' chain (xlm/eth/sol/btc).
- [x] Coinbase native withdraw endpoint (`POST /api/v3/brokerage/withdraw` → transfer hub).
- [x] durable deposit cursor (persist across restart, atomic write).
- [x] persisted terminal-order history (Coinbase historical endpoint now returns FILLED/CANCELLED).
- [~] USDC transfers — code path complete (Stellar CreditAsset / EVM ERC20); live needs a
      USDC-funded hot wallet (trustline + Circle key). See EXTRA-TESTING.md.
- [x] live ccxt-go Coinbase signed conformance (ES256 JWT) — PASS (`conformance/ccxt-go` coinbase mode)
- [x] Binance @depth incremental diffs; Coinbase fee fields; Solana **USDC SPL send** (TransferChecked,
      vector + RPC-fake tested). EVM ERC20 + Stellar CreditAsset USDC sends already done.
- [x] Solana SPL **deposit-watch** auto-credit — `Received` inspects pre/post token balances and
      emits a USDC payment (else SOL), so the hub credits the destination ledger. Full Solana USDC
      loop is code-complete (send + watch); vector/RPC-fake tested (`TestReceived_USDC`).
- [ ] (follow-up) live broadcast/verification of EVM/SOL/BTC sends (faucet/captcha-gated, unverifiable
      offline); EVM/BTC deposit-watch for USDC are ERC20-log / address-based and similar if needed.

## Phase 11 — Hardening & observability
- [x] Prometheus-text metrics (`internal/metrics`, dependency-free): orders/trades/cancels by edge, feed events, converge/RTR/tape/toxicity, per-instrument synthetic/anomalies/crossings/stale/VPIN/λ gauges; `:9090/metrics`
- [x] API rate limiting (`internal/ratelimit` token bucket + capped keyed limiter; wired on both REST edges → 429/-1003); config validation (`config.Validate()` fail-fast)
- [x] brutal review + hardening (KeyedLimiter maxKeys cap)
- [x] scenario golden-file test in CI (deterministic replay→reference→seeder→user→tape pinned to a committed golden; `UPDATE_GOLDEN=1` regenerates)
- [x] gRPC + native-WS request metrics (`exchange_{grpc,ws}_requests/commands_total` by method/command+status) + latency **histograms** (dependency-free `metrics.Histogram`)
- [x] README refresh with run instructions (branded README + capabilities)
- [x] account balance ledger → live /account + /accounts (lock/settle on trade)
