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
- [ ] `internal/feed/types.go`: `Trade`, `LOBSnapshot`, `LOBLevel`, `Ticker`, `Event`
- [ ] `internal/feed/source.go`: `Source` interface (`Start/Name/Status`)
- [ ] `internal/feed/binance/`: port `@depth20@100ms` + `@trade` (channel-based)
- [ ] `internal/feed/coinbase/`: port `market_trades`; implement `level2` parse+emit
- [ ] `internal/feed/replay/`: file-backed source (deterministic)
- [ ] feed record mode → write fixtures under `testdata/feed/`
- [ ] `cmd/feedcat/`: print live normalized trades+book
- [ ] unit tests (parse fixtures → expected events)

## Phase 2 — Reference book
- [ ] `internal/reference/book.go`: per-instrument LOB, snapshot+diff apply
- [ ] sequence-gap detection + resync, staleness flag
- [ ] `BestBidAsk`, `Depth(n)`, `Mid`, `Spread`, immutable snapshot
- [ ] tests vs recorded feed fixtures (deterministic book state)

## Phase 3 — Emulator seeding
- [ ] `internal/emulator/seeder.go`: reference levels → synthetic resting orders (tagged)
- [ ] reconcile loop (add/cancel/resize to match reference)
- [ ] config: venue, instruments, depth, refresh interval
- [ ] tests: no user activity ⇒ engine book ≈ reference

## Phase 4 — Return-to-Reference [a]
- [ ] `internal/emulator/rtr.go`: drain stale synthetics first
- [ ] progressive convergence over `tau` (exp decay of deviation)
- [ ] track spot moves (shift synthetic liquidity)
- [ ] scenario test: perturb → converge within `tau` (seeded, deterministic)

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

## Phase 7 — Binance-compatible API
- [ ] `internal/api/binance/rest.go`: order, openOrders, depth, ticker, account
- [ ] HMAC-SHA256 signature emulation + timestamp/recvWindow
- [ ] `internal/api/binance/ws.go`: market streams + user-data (executionReport)
- [ ] symbol/precision mapping
- [ ] test with `python-binance`/curl

## Phase 8 — Coinbase-compatible API
- [ ] `internal/api/coinbase/rest.go`: create/cancel/list orders, product book, ticker
- [ ] JWT/HMAC auth emulation
- [ ] `internal/api/coinbase/ws.go`: level2, market_trades, user channels
- [ ] test with Coinbase Advanced Trade client/curl

## Phase 9 — Custody examples (stretch, testnet only)
- [ ] `internal/custody/chain.go`: `Chain` interface (address, deposits, withdraw)
- [ ] XLM (Horizon testnet)
- [ ] Solana (devnet)
- [ ] ERC20 (Sepolia)
- [ ] wire balances into account endpoints; off-by-default flag; keys via env only

## Phase 10 — Hardening & observability
- [ ] Prometheus metrics (book deviation, λ/VPIN, fills, feed lag)
- [ ] structured logging, config validation, API rate limiting
- [ ] scenario + golden-file tests in CI
- [ ] README refresh with run instructions
