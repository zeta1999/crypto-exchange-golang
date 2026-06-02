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
- [ ] (perf, deferred) Div → limb math (256÷128, Knuth D) — rare path, big.Int for now
- [ ] (optional) float64-backed backend behind same API for A/B + fast mode
- [x] migration (PLAN.md §9.8): matching core (orderbook/engine/margin) → reference (parses feed decimal strings) → emulator → API edges convert at boundary; feed stays float64. CI green; live-verified (HTTP emits exact decimal strings). Follow-up: exact alpha=1 snap + 1e-9 convergence tolerance.

## Phase 5 — Trade replay sync
- [x] `internal/emulator/replay.go`: tape trade → IOC marketable order vs engine book (single-lock, no resting remainder)
- [x] fill user limits in sync with tape price (user fills at own price by priority; synthetic absorbs rest → RTR refills)
- [x] wired into binary: feed subscribes trades; per-instrument tape goroutines; tape orders margin-exempt
- [x] tests: user limit fills consistent with tape (buy/sell), price-capped, IOC no-remainder, non-finite/unknown-side guards
- [x] brutal review + fixes (IOC primitive vs place-then-cancel, NaN/Inf panic guard, HOL decouple)
- [ ] real-time vs accelerated (`speed`) clock — deferred to Phase 7 (trace replay drives the clock)

## Phase 6 — Configurable toxicity [b] — DONE
- [x] `internal/toxicity/kyle.go`: signed-volume → Δprice regression (λ)
- [x] `internal/toxicity/vpin.go`: volume buckets, informed-trade proxy (single-print capped)
- [x] `internal/emulator/toxic.go`: adverse-selection injector (seeded; scale·Score prob; scale·Impact penetration bounded to 1 spread)
- [x] config knobs (`scale`/`kyle_weight`/`vpin_weight`/`window_trades`/`bucket_volume`/`buckets`/`seed`) + wired into binary
- [x] tests: high toxicity ⇒ picks off resting user order; `scale:0` ⇒ pure RTR; non-finite guard
- [x] brutal review + fixes (bounded sweep, panic guard, weight/VPIN clamps)

## Phase 7 — Scenario & fault injection (OMS / strategy test bed)
- [x] Trace replay (full): `venue: replay` feeds the whole emulator from a recorded trace, offline + deterministic (reuses Phase 1 replay.Source; integration-tested + live-verified). `speed` pacing reserved/deferred.
- [x] `internal/emulator/latency.go`: artificial latency — feed→book (wired), order_ack/fill_report (TODO: apply at API edges, Phase 8/9), per-edge, jitter
- [x] `internal/emulator/priceshift.go`: artificial price shift — `offset_bps` + `scale` per venue (wired into dispatcher; shifts both float Price and PriceDecimal)
- [ ] cross-venue dislocation harness (two venues driven apart → closeable arbitrage)
- [x] scenario scripting format (JSONL): timeline of injection events (`scenario.go`; runtime-mutable `Controls`; price_shift+latency actions; deterministic; reviewed)
- [x] config: `emulator.{latency,price_shift,scenario}`
- [ ] tests: zeroed controls = no-op; injected latency shows in ack/fill timestamps;
      seeded scenario reproduces bit-for-bit; arb scenario is exploitable then closes

## Protocol compliance (cross-cutting, Phases 8–9)
- [ ] Validate the Binance/Coinbase-compatible API edges against a **real exchange-client
      library** pointed at the emulator with **only the endpoint/base-URL changed** — the
      library is the conformance oracle (if a stock client trades against us unmodified, we're
      compliant). Candidates:
  - **CCXT** (JS/Python, `ccxt`) — broadest coverage; set `exchange.urls['api'] = <emulator>`.
  - **GoEx / GoCryptoCurrencies** (Go, e.g. `github.com/nntaoli-project/goex`) — keeps the
      test client in-repo/in-language; point its REST/WS base at the emulator.
- [ ] Prefer an **unmodified** client (endpoint swap only); if a fork is unavoidable, vendor a
      minimal copy and document exactly what changed (ideally just the base URL / TLS skip).
- [ ] Use the chosen client to drive conformance tests: place/cancel/query orders, stream
      depth/trades + user data; diff responses vs the real venue's documented shapes.

## Phase 8 — Binance-compatible API
- [x] `internal/api/binance/rest.go`: order POST/DELETE, openOrders, depth, ticker, account (stub balances)
- [x] HMAC-SHA256 signature emulation + timestamp/recvWindow (constant-time; -1022/-1021/-2014/-2015)
- [x] symbol mapping (config BTCUSDT↔BTC-USD); registry w/ hook-driven fill tracking; wired behind config
- [x] tests (23) + brutal review + fixes (panic guard, phantom-record rollback); live-verified signed order
- [x] `internal/api/binance/ws.go`: market streams (@trade/@depth20) + user-data executionReport (listenKey)
- [ ] /exchangeInfo, per-symbol precision filters, real balances — deferred
- [ ] latency injection (Phase 7) applied at this edge (order_ack/fill_report)
- [ ] conformance: drive with CCXT / GoEx (endpoint-swapped); also `python-binance`/curl

## Phase 9 — Coinbase-compatible API
- [x] `internal/api/coinbase/rest.go`: create/batch_cancel/list orders (historical), product_book, products/ticker, accounts (stub)
- [x] HMAC auth emulation (CB-ACCESS-*, base64-or-raw secret, ±30s window); JWT/ES256 deferred
- [x] product allow-list; registry w/ hook fill tracking; record-before-place + rollback; wired behind config (:8083)
- [x] tests (31) + brutal review (clean, no fixes needed); live-verified signed create + list
- [x] `internal/api/coinbase/ws.go`: level2, market_trades, user channels (message-based subscribe)
- [ ] JWT/ES256 production auth, fee/precision fields, persisted terminal-order history — deferred
- [ ] conformance: drive with CCXT / GoEx (endpoint-swapped); also Coinbase Advanced Trade client/curl

## Phase 10 — Custody examples (stretch, testnet only)
- [ ] `internal/custody/chain.go`: `Chain` interface (address, deposits, withdraw)
- [ ] XLM (Horizon testnet)
- [ ] Solana (devnet)
- [ ] ERC20 (Sepolia)
- [ ] wire balances into account endpoints; off-by-default flag; keys via env only

## Phase 11 — Hardening & observability
- [x] Prometheus-text metrics (`internal/metrics`, dependency-free): orders/trades/cancels by edge, feed events, converge/RTR/tape/toxicity, per-instrument synthetic/anomalies/crossings/stale/VPIN/λ gauges; `:9090/metrics`
- [x] API rate limiting (`internal/ratelimit` token bucket + capped keyed limiter; wired on both REST edges → 429/-1003); config validation (`config.Validate()` fail-fast)
- [x] brutal review + hardening (KeyedLimiter maxKeys cap)
- [ ] scenario + golden-file tests in CI (RTR, toxicity, fault injection)
- [ ] gRPC/native-WS request metrics; injected-latency histograms
- [ ] README refresh with run instructions
