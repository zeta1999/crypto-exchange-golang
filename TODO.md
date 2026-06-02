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
- [ ] (perf, deferred) replace big.Int Mul/Div with allocation-free limb math + benchmark (PLAN §9.7)
- [ ] (optional) float64-backed backend behind same API for A/B + fast mode
- [ ] migration (PLAN.md §9.8): matching core → feed edge → reference → emulator → WAL/API

## Phase 5 — Trade replay sync
- [ ] `internal/emulator/replay.go`: tape trade → marketable order vs engine book
- [ ] fill user limits in sync with tape timing/price
- [ ] clock model: real-time + accelerated (`speed`)
- [ ] test: user limit at touched price fills consistent with tape

## Phase 6 — Configurable toxicity [b]
- [ ] `internal/toxicity/kyle.go`: signed-volume → Δprice regression (λ)
- [ ] `internal/toxicity/vpin.go`: volume buckets, informed-trade proxy
- [ ] adverse-selection injector on resting user limits
- [ ] config knobs: `scale`, `kyle_weight`, `vpin_weight`, `window_trades`, `seed`
- [ ] tests: high toxicity ⇒ more adverse fills; `scale:0` ⇒ pure RTR

## Phase 7 — Scenario & fault injection (OMS / strategy test bed)
- [ ] Trace replay (full): feed whole emulator from a recorded trace, deterministic, `speed`
- [ ] `internal/emulator/latency.go`: artificial latency — feed→book, order_ack, fill_report, per-edge, jitter
- [ ] `internal/emulator/priceshift.go`: artificial price shift — `offset_bps` + `scale` per venue
- [ ] cross-venue dislocation harness (two venues driven apart → closeable arbitrage)
- [ ] scenario scripting format (YAML/JSONL): timeline of injection events
- [ ] config: `emulator.{latency,price_shift,scenario}`
- [ ] tests: zeroed controls = no-op; injected latency shows in ack/fill timestamps;
      seeded scenario reproduces bit-for-bit; arb scenario is exploitable then closes

## Phase 8 — Binance-compatible API
- [ ] `internal/api/binance/rest.go`: order, openOrders, depth, ticker, account
- [ ] HMAC-SHA256 signature emulation + timestamp/recvWindow
- [ ] `internal/api/binance/ws.go`: market streams + user-data (executionReport)
- [ ] symbol/precision mapping
- [ ] latency injection (Phase 7) applied at this edge
- [ ] test with `python-binance`/curl

## Phase 9 — Coinbase-compatible API
- [ ] `internal/api/coinbase/rest.go`: create/cancel/list orders, product book, ticker
- [ ] JWT/HMAC auth emulation
- [ ] `internal/api/coinbase/ws.go`: level2, market_trades, user channels
- [ ] test with Coinbase Advanced Trade client/curl

## Phase 10 — Custody examples (stretch, testnet only)
- [ ] `internal/custody/chain.go`: `Chain` interface (address, deposits, withdraw)
- [ ] XLM (Horizon testnet)
- [ ] Solana (devnet)
- [ ] ERC20 (Sepolia)
- [ ] wire balances into account endpoints; off-by-default flag; keys via env only

## Phase 11 — Hardening & observability
- [ ] Prometheus metrics (book deviation, λ/VPIN, fills, feed lag, injected-latency histograms)
- [ ] structured logging, config validation, API rate limiting
- [ ] scenario + golden-file tests in CI (RTR, toxicity, fault injection)
- [ ] README refresh with run instructions
