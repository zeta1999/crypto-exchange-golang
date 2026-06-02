# PLAN — Configurable Exchange Emulator

> Goal: turn this exchange skeleton into a **configurable exchange emulator** that
> mirrors real Coinbase/Binance markets in real time, runs a real matching engine,
> and exposes **Binance- and Coinbase-compatible APIs** so existing clients/bots can
> trade against it as if it were the real venue.

## 1. Vision

We emulate a live venue by anchoring our internal limit order book (LOB) to a **reference
book** rebuilt from a real exchange's websocket feed. User orders match against a real
engine. Two control mechanisms make the emulation realistic:

- **[a] Return-to-Reference (RTR):** A user trade at `T+1` perturbs the book. The
  emulator then *progressively converges* the local LOB back toward the live reference
  feed, draining stale synthetic limit orders first and replaying real tape trades in
  sync (similar timing/price) when the spot moves.
- **[b] Configurable market toxicity:** When a user posts a limit order, the market may
  "move and execute against you" with a tunable probability/impact. Driven by simple,
  tape-induced models — **Kyle's λ** (price impact per unit volume) and **VPIN**
  (volume-synchronized probability of informed trading) — with **over/under-weight knobs**.

Optional stretch: **custody examples** showing deposit/withdraw + balance settlement on
**XLM**, **Solana**, and **ERC20** (testnets only).

### Primary use case: a test bed for trading / OMS systems

`mirage` is built first and foremost as a **deterministic, scriptable sandbox** to run an
OMS or trading strategy against — for technical tests (connectivity, order lifecycle,
reconnection, backpressure) and scenario tests (regime changes, dislocations, stress). To
that end it provides three **fault/scenario-injection** controls in addition to [a]/[b]:

- **Trace replay:** replay past market events from recorded traces — deterministic and
  repeatable, at real-time or accelerated `speed`.
- **Artificial latency:** inject configurable delays (feed→book, order ack, fill report,
  per-API-edge, with jitter) to exercise how the system under test handles slow/racy venues.
- **Artificial price shift:** offset/scale the reference price per venue to manufacture
  **cross-venue dislocations** — a controlled lab for testing **arbitrage** and
  relative-value models.

## 2. Design principles

- **Stay simple at each step.** Prefer the smallest model that produces believable behavior.
- **Deterministic core, stochastic edges.** Matching is deterministic; toxicity/RTR use a
  seedable RNG so runs are reproducible.
- **Config-driven.** Every behavior (which venue to mirror, toxicity weights, RTR speed)
  lives in `configs/*.yaml`; no recompile to retune.
- **Compatibility is an adapter, not a rewrite.** The core engine stays venue-neutral;
  Binance/Coinbase API shapes are translation layers on top.
- **Reuse, don't reinvent.** Feed clients come from `../this-is-not-bbg`.

## 3. Current foundation (what we start from)

Working skeleton (see git history): price-time matching engine (`internal/engine`,
`internal/orderbook`), gRPC/HTTP/WS APIs (`internal/api/*`), WAL (`pkg/wal`), config
(`pkg/config`), token auth (`pkg/auth`), concurrent dict experiments
(`internal/concurrency/dict`, mostly unused). Margin validator is a stub.

Feed source `../this-is-not-bbg` (module `github.com/notbbg/notbbg/server`) has Binance
(`@trade`, `@kline`, `@depth20@100ms`) and Coinbase (`market_trades`, `candles`, `level2`
subscribed-but-unpublished) adapters in `internal/feeds/ccxt`. They're `internal/` so not
directly importable — we extract them into our own feed package.

## 4. Architecture (target)

```
            ┌──────────────────────────────────────────────────────┐
 Real venue │  feed/ (Binance, Coinbase WS adapters, normalized)    │
   WS  ───► │   → ReferenceBook (snapshot + diffs, per instrument)  │
            │   → TradeTape (real executions, timestamped)          │
            └───────────────┬───────────────────────┬──────────────┘
                            │ reference LOB          │ real trades
                            ▼                         ▼
            ┌──────────────────────────────────────────────────────┐
 Emulator   │  emulator/                                            │
   core     │   • Seeder: mirror reference liquidity as synthetic   │
            │     resting orders in the engine book                 │
            │   • RTR controller [a]: converge book → reference     │
            │   • Toxicity model [b]: Kyle λ / VPIN adverse-select  │
            │   • Replay: inject real tape trades in sync           │
            └───────────────┬──────────────────────────────────────┘
                            ▼
            ┌──────────────────────────────────────────────────────┐
 Matching   │  internal/engine + internal/orderbook (real matching) │
            └───────────────┬──────────────────────────────────────┘
                            ▼
            ┌──────────────────────────────────────────────────────┐
 API edge   │  api/binance  (REST + WS, Binance-compatible)         │
            │  api/coinbase (REST + WS, Coinbase Adv-Trade-compat)  │
            │  existing gRPC/HTTP/WS (native)                       │
            │  custody/ (XLM, Solana, ERC20 — optional)             │
            └──────────────────────────────────────────────────────┘
```

## 5. Phased plan

Each phase is small and shippable. **Definition of Done (DoD)** per phase: code compiles,
`./ci.sh` green, unit tests for new logic, STATUS/TODO updated, brutal code review by
subagent addressed, manual TESTING steps for the phase pass. Commit after each phase
(**do not push**).

---

### Phase 0 — Foundations, CI, docs (no behavior change)
- Add local `ci.sh` (gofmt check, `go vet`, golangci-lint if present, `go build ./...`,
  `go test ./... -race -count=1`).
- Add PLAN.md, STATUS.md, TODO.md, TESTING.md (this commit).
- Add `Makefile` targets (`make ci`, `make run`, `make test`).
- Confirm baseline: existing tests pass.
- **DoD:** `./ci.sh` green on untouched code.

### Phase 1 — Feed ingestion layer
- Create `internal/feed/` with a normalized market-data API:
  `Trade`, `LOBSnapshot`, `LOBLevel`, `Ticker` (copy shapes from this-is-not-bbg).
- Define `Source` interface: `Start(ctx) (<-chan Event, error)`, `Name()`, `Status()`.
- Port Binance adapter (`@depth20@100ms` + `@trade`) — minimal, channel-based (drop the bus).
- Port Coinbase adapter (`market_trades`); **enable `level2`** parsing + emit (the gap in
  the source repo).
- Add a `replay` source: read recorded feed from file (for offline/deterministic tests).
- Add `record` mode: persist live feed to disk for fixtures.
- **DoD:** `cmd/feedcat` prints live normalized trades+book for BTC-USD from both venues;
  replay reproduces a recorded session.

### Phase 2 — Reference book
- `internal/reference/`: maintain a per-instrument **ReferenceBook** from feed
  (snapshot + incremental diffs for Binance depth; rebuild for Coinbase level2).
- Sequence-gap detection + resync; staleness flag.
- Expose `BestBidAsk()`, `Depth(n)`, `Mid()`, `Spread()`, immutable snapshot read.
- **DoD:** ReferenceBook tracks live BTC-USD within tolerance vs raw feed; replay test
  asserts deterministic book state at each step.

### Phase 3 — Emulator seeding (mirror reference into engine)
- `internal/emulator/`: **Seeder** maps ReferenceBook levels → synthetic resting limit
  orders in the engine book (tagged `synthetic`, distinct from user orders).
- Re-seed on cadence; reconcile (add/cancel/resize synthetic orders to match reference).
- Config: which venue/instruments to mirror, depth levels, refresh interval.
- **DoD:** With no user activity, engine book ≈ reference book continuously; user can
  query our native snapshot and see live-like liquidity.

### Phase 4 — Return-to-Reference (RTR) controller [a]
- After a user trade perturbs the book, drive convergence back to reference:
  1. **Drain stale synthetic limits first** (remove RT-derived levels no longer in feed).
  2. **Re-converge progressively** over a configurable horizon (not instantly).
  3. When spot moves in the feed, **shift synthetic liquidity** to track it.
- Simple model first: exponential decay of the deviation between engine book and reference
  over `tau` (config). Knob: convergence speed.
- **DoD:** Scripted scenario — seed book, submit user trade, observe book converge to
  reference within `tau`; deterministic under replay+seed.

### Phase 5 — Trade replay sync
- `Replay` injects real tape trades into the emulator so user orders resting in the book
  can be filled **in sync** with real executions (matching timing/price when sensible).
- Map feed trade (price, size, side, ts) → marketable order against the engine book; if a
  user limit sits at/through that price, it fills like it would on the real venue.
- Clock model: real-time or accelerated (config `speed`).
- **DoD:** Replay a recorded session; user limit order at a touched price gets filled at a
  time/price consistent with the tape.

### Phase 6 — Configurable toxicity [b]
- `internal/toxicity/`: tape-induced estimators, recomputed on a rolling window:
  - **Kyle's λ**: regress signed-volume → price change ⇒ impact coefficient.
  - **VPIN**: volume buckets, |buy−sell|/bucketVol ⇒ informed-trading proxy.
- **Adverse selection on user limit orders:** probability/aggressiveness that the market
  "comes and takes" a resting user order scales with λ and VPIN.
- Config knobs: `toxicity.kyle_weight`, `toxicity.vpin_weight`, global `toxicity.scale`
  (over/under-weight, incl. 0 = off). Seedable RNG.
- **DoD:** With toxicity high, resting user limits get adversely filled more often (and
  near unfavorable prints); with `scale: 0`, behavior reduces to pure RTR. Stats logged.

### Phase 7 — Scenario & fault injection (OMS / strategy test bed)
The test-bed core: deterministic, scriptable controls to drive the system under test.
- **Trace replay (full):** record live feeds to traces (Phase 1 groundwork) and replay
  them deterministically through the whole emulator (reference + seeding + RTR + toxicity),
  at real-time or accelerated `speed`. Reproducible with a fixed seed.
- **Artificial latency:** `internal/emulator/latency.go` injects configurable delays at
  defined points — `feed_to_book_ms`, `order_ack_ms`, `fill_report_ms`, per-API-edge, plus
  `jitter_ms`. Used to test OMS behavior under slow/racy venues and ack/fill ordering.
- **Artificial price shift:** `internal/emulator/priceshift.go` applies an `offset_bps`
  and/or `scale` to a venue's reference price, so two emulated venues can be driven apart to
  manufacture **cross-venue dislocations** — a controlled lab for **arbitrage** and
  relative-value strategy testing.
- **Scenario scripting:** a small scenario format (YAML/JSONL) to sequence events — "at
  t=10s shift BTC-USD +15bps", "add 50ms order-ack latency", "replay trace X at 4×".
- **DoD:** A scripted scenario reproduces bit-for-bit across runs; an OMS under test sees
  injected latency in its ack/fill timestamps; two venues with a configured price gap
  expose a closeable arbitrage; all controls reduce to no-ops when zeroed.

### Phase 8 — Binance-compatible API
- `internal/api/binance/`: REST subset (`/api/v3/order` POST/DELETE, `/api/v3/openOrders`,
  `/api/v3/depth`, `/api/v3/ticker/price`, `/api/v3/account`) + user-data & market WS
  streams (`@depth`, `@trade`, executionReport). HMAC-SHA256 signing emulation.
- Map Binance symbols/precision to internal instruments. Latency injection (Phase 7) applies
  at this edge.
- **DoD:** `python-binance` (or curl) can place/cancel/query orders and stream depth
  against the emulator.

### Phase 9 — Coinbase-compatible API
- `internal/api/coinbase/`: Advanced Trade REST subset (create/cancel order, list orders,
  product book, ticker) + WS (`level2`, `market_trades`, `user`). JWT/HMAC auth emulation.
- **DoD:** A Coinbase Advanced Trade client (or curl with signed headers) trades and
  streams against the emulator.

### Phase 10 — Custody examples (optional / stretch)
- `internal/custody/` with pluggable chains, **testnet only**, deposit-address generation,
  balance crediting on confirmation, withdrawal signing:
  - **XLM** (Horizon testnet), **Solana** (devnet), **ERC20** (Sepolia).
- Wire balances into account endpoints so deposits fund trading.
- **DoD:** A testnet deposit credits a balance; a withdrawal broadcasts a testnet tx.

### Phase 11 — Hardening & observability
- Metrics (Prometheus): book deviation vs reference, λ/VPIN gauges, fill counts, feed lag,
  injected-latency histograms.
- Structured logging, config validation, rate limiting on API edges, graceful resync.
- Scenario harness + golden-file tests for RTR, toxicity, and fault injection.
- **DoD:** Dashboards/logs show emulation health; CI includes scenario tests.

## 6. Configuration sketch (`configs/dev.yaml` additions)

```yaml
emulator:
  enabled: true
  venue: coinbase          # coinbase | binance
  instruments: ["BTC-USD", "ETH-USD"]
  reference:
    depth_levels: 20
    refresh_ms: 250
  rtr:                     # [a] return-to-reference
    tau_ms: 3000           # convergence horizon
    drain_stale_first: true
  toxicity:                # [b]
    scale: 1.0             # 0 = off, >1 over-weight, <1 under-weight
    kyle_weight: 1.0
    vpin_weight: 1.0
    window_trades: 500
    seed: 42
  replay:                  # trace replay (test bed)
    mode: live             # live | file
    file: ""               # recorded trace, used when mode=file
    speed: 1.0             # playback multiplier (accelerated scenarios)
  latency:                 # artificial latency injection (test bed)
    feed_to_book_ms: 0
    order_ack_ms: 0
    fill_report_ms: 0
    jitter_ms: 0
  price_shift:             # artificial price shift — manufacture cross-venue arb (test bed)
    offset_bps: 0          # additive shift in basis points
    scale: 1.0             # multiplicative shift (1.0 = none)
  scenario:                # optional scripted timeline of injection events (Phase 7)
    file: ""               # scenario YAML/JSONL; empty = none
api:
  binance:  { enabled: false, listen: ":8082" }
  coinbase: { enabled: false, listen: ":8083" }
```

## 7. Risks & open questions

- **Feed reuse:** adapters are `internal/` in this-is-not-bbg → must copy/vendor, not
  import. Keep a clear provenance note + license check.
- **Coinbase level2** isn't published in the source repo — we implement parsing ourselves.
- **Realism vs simplicity:** Kyle/VPIN are crude; acceptable for an emulator, documented.
- **API fidelity:** full Binance/Coinbase parity is large — we ship a documented subset.
- **Custody:** real key handling even on testnet is sensitive — keep keys in env, never
  commit, testnet-only, behind an off-by-default flag.

## 8. Non-goals

- Production-grade exchange (settlement finality, full regulatory surface).
- Mainnet custody / real funds.
- 100% API coverage of either venue.

## 9. Planned: fixed-point decimal arithmetic

> **TODO (foundational).** Replace `float64` prices/quantities with an exact base-10
> fixed-point `Decimal` so matching is bit-deterministic and venue decimal strings round-trip
> losslessly. This is a detailed Go design translated from a reference C++ fixed-point type.

### 9.1 Motivation
Prices and volumes are `float64` throughout (`feed`, `reference`, `orderbook`, `engine`,
`emulator`). Floats can't represent decimal venue prices exactly (e.g. `0.1`), accumulate
rounding under add/multiply, and make the `volEps` tolerances in the emulator necessary. The
feed already preserves the venue's exact strings (`PriceDecimal`/`QuantityDecimal`); an exact
`Decimal` lets the matching core consume those losslessly and match deterministically.

### 9.2 Type and storage (`pkg/decimal`, no external deps)
`Decimal` is a base-10 fixed-point number with **18 fractional digits**, stored as a **signed
128-bit integer of scaled units**: `value = raw / 10^18`.

- `ScaleDigits = 18`; `Scale = 10^18 = 1_000_000_000_000_000_000` — note `10^18 ≈ 1.15e18 < 2^63`,
  so the *scale itself* fits in `int64`/`uint64`, but a *scaled value* (e.g. `70000.0 → 7e22`)
  does not — hence 128-bit storage.
- Go has no native `int128`. Store as a two's-complement 128-bit integer:
  `type Decimal struct { hi int64; lo uint64 }`. Range ≈ ±1.7e38; with the 10^18 scale the
  integer part spans ≈ ±1.7e20 — ample for crypto prices and sizes. `Decimal` stays a
  `comparable` value type (usable directly as a map key — replaces the float-formatted level
  keys in `reference`/`emulator`).

### 9.3 Construction
- `FromRaw(hi int64, lo uint64) Decimal` / `(d Decimal) Raw() (int64, uint64)` — direct scaled
  access (WAL, wire, hashing).
- `FromInt(n int64) Decimal` → `raw = n * Scale` (128-bit multiply).
- `FromFloat(f float64) Decimal` → `round(f * Scale)`; **lossy**, ingestion/convenience only.
- `Parse(s string) (Decimal, error)` — exact, mirroring the reference state machine: optional
  `+`/`-`; one or more integral digits; optional `.` then up to 18 fractional digits (extra
  digits truncated, missing digits zero-padded to 18); `raw = integral*Scale + fractional`,
  negate if signed; error on any malformed input or trailing garbage.
- `MustParse(s string) Decimal` — panics on error (constants/tests).

### 9.4 Conversion / formatting
- `Float64() float64` — lossy.
- `String() string` and `StringPrec(prec int) string` — clamp `prec` to `[0,18]`; split `|raw|`
  into `integral = |raw|/Scale` and `frac = |raw|%Scale`; render `frac` to 18 digits then
  trim to `prec` (**truncate**, matching the reference; document the rounding choice); prepend
  `-` if negative. Default precision 6 for human output, but the matching core should serialize
  full precision.
- `MarshalJSON`/`UnmarshalJSON` and `MarshalText`/`UnmarshalText` as the **decimal string**
  (preserves exactness on the wire; aligns with the feed's `*Decimal` fields and the WAL).

### 9.5 Arithmetic
- `Add`, `Sub`: 128-bit two's-complement add/sub via `math/bits.Add64`/`Sub64` (carry/borrow);
  no scale change.
- `Neg`, `Abs`, `Sign() int`, `IsZero() bool`.
- `Mul(a, b)`: result `= a*b / Scale`. The full product of two 128-bit values is up to 256-bit,
  so compute a **128×128→256** product (split into 64-bit limbs with `math/bits.Mul64` +
  `Add64` accumulation, tracking sign on magnitudes), then divide the 256-bit magnitude by
  `Scale` (a 64-bit divisor) back to 128-bit; reapply sign.
- `Div(a, b)`: result `= a*Scale / b`. Numerator `a*Scale` is up to ~188-bit → use a 256-bit
  intermediate, divide by `b` (128-bit) → 128-bit. **Division by zero panics** (programmer
  error). Document truncation toward zero.
- **Overflow policy:** the 256-bit intermediates make mul/div overflow-safe across the full
  128-bit *input* range; if a *result* exceeds 128 bits, panic with a clear message (don't
  silently wrap). Document.
- Convergence/scaling helpers the emulator needs: represent factors like RTR's `alpha` as a
  `Decimal` and use `Mul`, or add `MulFloat(f float64)` for non-exact scaling at edges.

### 9.6 Comparisons & helpers
- `Cmp(a, b) int` (−1/0/1): compare `hi` as signed, then `lo` as unsigned. `Eq`/`Lt`/`Lte`/
  `Gt`/`Gte`. Free functions `Min`, `Max`, `Abs`.

### 9.7 Backends (optional; mirrors the reference Int128 / Wide / Double variants)
- **Exact (default):** 128-bit storage with 256-bit mul/div intermediates — always
  overflow-safe (the reference's "Wide" behavior; we do **not** ship the overflow-prone
  128-bit-only multiply).
- **Float (optional):** a `float64`-backed type behind the same interface/alias for A/B
  benchmarking and a deliberately fast/loose mode. Keep callers backend-agnostic via an
  interface or build-tagged alias so the choice is one line.
- **Implementation order:** ship a correct-but-simple first cut using `math/big.Int` for the
  256-bit intermediate in `Mul`/`Div` (allocates), behind the final API; then replace the hot
  paths with allocation-free 64-bit-limb math (`math/bits`) and benchmark.

### 9.8 Migration (large, cross-cutting — sequence carefully)
1. Land `pkg/decimal` with full tests (no callers yet).
2. Convert the matching core: `orderbook.Order.Price/Volume`, `orderbook.Level`, `engine` →
   `Decimal`. Matching becomes exact ⇒ fully deterministic.
3. Feed edge: `Parse` the already-exact `PriceDecimal`/`QuantityDecimal` → `Decimal`; keep
   `float64` only for display/metrics.
4. `reference.Book`: key levels by `Decimal` (drop the float-formatted key), volumes `Decimal`,
   re-examine `Crossed`/`Mid`/`Spread`.
5. `emulator` seeder/RTR: `Decimal` volumes; `volEps` tolerance becomes exact equality (revisit
   the no-churn check) since fold/convergence are now exact.
6. WAL/API payloads serialize `Decimal` as strings.

### 9.9 Testing
- `Parse` ↔ `String` round-trip over random decimals and edge formats.
- Arithmetic property-checked against a `math/big.Rat` oracle over random inputs.
- Edges: max/min representable, division-by-zero panic, negatives, 18-digit truncation,
  result-overflow panic.
- JSON/WAL round-trip; map-key usage.

See `STATUS.md` for progress, `TODO.md` for the granular checklist, `TESTING.md` for the
manual test plan, and `ci.sh` for the local CI gate.
