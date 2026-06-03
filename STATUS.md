# STATUS

_Last updated: 2026-06-02_

## Current phase
**Runnable product + exact decimals** ✅ — `go run ./cmd/exchange` boots a live Coinbase-mirroring
exchange (feed → reference → seeded synthetic liquidity + RTR), tradable via gRPC/HTTP/WS, with
all prices/quantities now exact `decimal.Decimal` (matching core + reference + emulator migrated;
API edges convert; feed stays float64). Verified live (book 20 levels/side, uncrossed; HTTP emits
exact decimal strings). Phases 1–7, `pkg/decimal`, and the float64→Decimal migration
done & reviewed. **Phase 7 complete:** price shift (cross-venue arb dislocations), latency
injectors (feed→book, order-ack sync, fill-report async), scenario scripting, full trace replay
+ speed pacing, cross-venue arb harness, and the deterministic clock (byte-reproducible runs) —
all wired, default-off, reviewed/tested.
**Near-complete.** Done & reviewed: Phases 1–7 (price-shift, latency incl. ack+fill,
scenario scripting, trace replay + speed pacing, cross-venue arb harness, deterministic
clock), Phases 8–9 (Binance + Coinbase REST + WS, incl. exchangeInfo/products loadMarkets
endpoints), `pkg/decimal` + migration, and Phase 11 hardening (metrics, rate limiting, config
validation). **CCXT conformance PASS:** the stock `ccxt-go` v4 binance client drives the edge
end-to-end (loadMarkets→fetchOrderBook→createLimitOrder→fetchOpenOrders→cancelOrder) with only
the base URL changed (`conformance/ccxt-go/`); surfaced + fixed two SIGNED-POST bugs (sign over
query+body, timestamp from body). **Remaining:** Phase 11 tail (scenario golden-file CI tests,
gRPC/WS request metrics); **Phase 10 custody** (stretch, testnet). Deferred polish: Coinbase
CCXT conformance run (JWT vs HMAC class choice).

## Legend
☐ not started ◐ in progress ☑ done

## Phase progress
| # | Phase | State | Notes |
|---|-------|-------|-------|
| 0 | Foundations, CI, docs | ☑ | docs + `ci.sh` + Makefile committed; baseline CI **green**; brutal review done + fixes applied |
| 1 | Feed ingestion layer | ☑ | channel-based `feed.Source`; Binance @trade+@depth20, Coinbase market_trades + from-scratch level2; replay+record; `cmd/feedcat`. CI green; **live-verified** both venues + deterministic replay; brutal review applied (determinism, book-integrity, liveness/reconnect, lifecycle test). |
| 2 | Reference book | ☑ | `internal/reference` Book (snapshot+diff, float-keyed levels, crossed-book detection, staleness) + Set (per-instrument routing, Consume). Coinbase connection-global seq-gap detection in adapter. CI green; live BTC-USD book uncrossed; review applied. |
| 3 | Emulator seeding | ☑ | `internal/emulator.Seeder` mirrors `reference.Book` → tagged synthetic engine orders; reconcile (cancel-before-place, resize, skip crossed), not-found-tolerant, Run/Clear. CI green; live BTC-USD top-20 mirrors exactly, 0 trades; review applied (proved no self-match). |
| 4 | Return-to-Reference [a] | ☑ | `Seeder.Converge(alpha)` (target=cur+α·(ref−cur)); fill accounting via generation-stamped IDs + trade hook; `RTR` exp-decay controller (α=1−e^(−dt/τ)). CI green; DoD scenario (user trade → gradual reconverge) deterministic; review applied. |
| 5 | Trade replay sync | ☑ | `internal/emulator.TapeReplay` injects tape via single-lock IOC (orderbook.ExecuteLimitIOC); resting user orders fill in sync at own price; wired into binary (per-instrument tape goroutines). CI green; live-verified; review applied (IOC, NaN guard). |
| 6 | Configurable toxicity [b] | ☑ | `internal/toxicity` Kyle λ + VPIN; `emulator.ToxicInjector` seeded adverse sweep (scale·Score prob, scale·Impact ≤1 spread); config knobs wired; `scale:0`=pure RTR. CI green; review applied (bounded, guarded). |
| 7 | Scenario & fault injection (test bed) | ☑ | price shift (arb dislocation) + latency (uniform/**Poisson**) + **scenario scripting** (JSONL timeline → runtime `Controls`) + **full trace replay** (`venue: replay` runs the whole emulator offline/deterministic from a recorded trace) + **replay `speed` pacing** (`WithSpeed`: <=0 fast/deterministic, 1.0 real time, Nx) + **cross-venue arb harness** (`ArbHarness` mirrors one reference into two shifted venues; `CrossArb` detector; test proves exploitable-then-closeable arb) + **deterministic clock** (`OrderBook.SetClock` + golden test, byte-reproducible runs) + **order-ack & fill-report latency at API edges** (`WithAckDelay` sync on handler; `WithFillDelay` async-holds the fill user-data push via `time.AfterFunc`, only TRADE/fill updates, NEW/cancel prompt; documented non-determinism caveat) done, wired, reviewed/tested. **Phase 7 complete.** |
| 8 | Binance-compatible API | ☑ | `internal/api/binance` REST (signed order/cancel/openOrders/account + ping/time/**exchangeInfo**/depth/ticker) **and WS** (market @trade/@depth20 + user-data executionReport via listenKey). HMAC auth, symbol map, registry w/ hook fills. **`exchangeInfo` unblocks CCXT loadMarkets** (symbols + PRICE_FILTER/LOT_SIZE/NOTIONAL). CI+race green; live-verified (incl. exchangeInfo); reviewed. Deferred: @depth diffs, real balances. |
| 9 | Coinbase-compatible API | ☑ | `internal/api/coinbase` Advanced Trade REST (signed orders/batch_cancel/historical/accounts + time/product_book/**products list**+single) **and WS** (level2/market_trades/user channels, message-based subscribe). CB-ACCESS HMAC **+ ES256 JWT** auth (ECDSA P-256, Bearer REST + WS), registry w/ hook fills (:8083). **`products` list unblocks CCXT loadMarkets** (base/quote ids + increments + min size + trading_disabled). CI+race green; live-verified; reviewed. Deferred: true level2 diffs, fee fields. |
| 10 | Custody examples (stretch) | ☑ | **Testnet wallet/faucet toolkit complete** (`internal/custody` + `cmd/custody`, off by default, NOT wired into the server). **Phase 1:** `internal/custody` + `cmd/custody` testnet wallet/faucet toolkit. Encrypted keystore (AES-256-GCM + PBKDF2, secrets encrypted at rest, passphrase via `CUSTODY_PASSPHRASE`); pluggable `Chain`/`Faucet` + testnet-only `MustTestnet` guard; **XLM** (ed25519/StrKey, friendbot, Horizon) + **SOL** (ed25519/base58, devnet airdrop, getBalance) + **ETH/ERC20** (Sepolia; secp256k1+keccak EIP-55 addresses, eth_getBalance + ERC20 balanceOf incl. Circle USDC; manual faucet). Encoders validated against real Circle USDC issuer/mint + privkey=1 + EIP-55 spec vectors; **live: friendbot funded a generated address w/ 10000 XLM; Sepolia returns ETH+USDC balances**. CI+race green; reviewed (KDF-param validation, lamports bounds, no fake-success, no RPC-blip balance masking). **Phase 3 done:** USDC on XLM + SOL — `prepare` establishes a Stellar trustline (live: signed+submitted a real changeTrust tx; balance then shows USDC) using the stellar/go SDK; SOL SPL-token balance reading; Circle drip faucet (`CIRCLE_API_KEY`, falls back to the web faucet, blockchain ids env-overridable). Deps: decred secp256k1 + x/crypto/sha3 + stellar/go. CI+race green; reviewed (secret handling clean, testnet passphrase/endpoint consistent, parse-error propagation, secret zeroing). **Phase 4:** Bitcoin testnet — secp256k1 + hand-rolled bech32 native-segwit (P2WPKH `tb1…`) addresses, Esplora balances, manual faucet. bech32 validated against the BIP-173 vector; **live: a generated tb1 address is accepted by real Esplora**. CI+race green. |
| 11 | Hardening & observability | ◐ | `internal/metrics` (dependency-free Prometheus-text registry, instrumented pipeline + API edges, `:9090/metrics`), `internal/ratelimit` (token bucket + keyed, capped; wired on both REST edges → 429/-1003), `config.Validate()` fail-fast. CI+race green; live-verified (metrics scrape, rate limiter trips). Remaining: scenario golden-file CI tests, gRPC/WS request metrics. |

## How to run
`EXCHANGE_CONFIG=configs/dev.yaml go run ./cmd/exchange` → live Coinbase mirror on
gRPC :50051 / HTTP(S) :8080 / WS :8081. Query a book:
`curl -sk -H "Authorization: Bearer dev-secret-token" https://localhost:8080/snapshot/BTC-USD`.
Set `emulator.enabled: false` for a plain offline matching engine.

## Baseline (inherited skeleton)
- ☑ Price-time matching engine (`internal/engine`, `internal/orderbook`)
- ☑ gRPC / HTTP / WS native APIs (`internal/api/*`)
- ☑ WAL (`pkg/wal`), config (`pkg/config`), token auth (`pkg/auth`)
- ◐ Margin validator (stub — notional limit example only)
- ☐ Concurrent dicts present but unused in hot path

## Decisions log
- 2026-06-02: CI is a **local `ci.sh`** script, not GitHub Actions.
- 2026-06-02: Feed adapters are **copied/vendored** from `../this-is-not-bbg` (they live in
  `internal/`, module `github.com/notbbg/notbbg/server`, not importable). Vendored into
  `internal/feed/` with a provenance note. **Confirmed: vendor, do not import.**
- 2026-06-02: Drop notbbg's pub/sub `bus.Bus`; use a plain channel-based `Source`
  interface (`Start(ctx) (<-chan Event, error)`, `Name()`, `Status()`).
- 2026-06-02: Coinbase `level2` is subscribed-but-unpublished upstream — we implement
  parse+emit ourselves (Phase 1).
- 2026-06-02: Module path stays `github.com/zeta1999/crypto-exchange-golang` (inherited).
- 2026-06-02: Binance/Coinbase API compatibility ships a **documented subset**, not full parity.
- 2026-06-02: Custody (XLM/Solana/ERC20) is **stretch, testnet-only, off by default**, keys via env.
- 2026-06-02: Determinism — matching deterministic; RTR + toxicity use a seedable RNG.
- 2026-06-02: Toxicity uses Kyle's λ + VPIN with a global `scale` knob (0 = off).
- 2026-06-02: **Primary use case = test bed for the user's trading/OMS system** (technical
  + scenario testing). Added Phase 7 (scenario & fault injection): **trace replay**,
  **artificial latency**, **artificial price shift** (manufacture cross-venue dislocations
  to test arbitrage / relative-value models). Phases renumbered 7→8…10→11.
- 2026-06-02: Project codename **mirage** (README + logo); easily renamed.
- 2026-06-02: **Protocol compliance via a real client library** (Phases 8–9): validate the
  Binance/Coinbase-compatible edges by pointing a stock exchange-client — **CCXT** or **GoEx
  (GoCryptoCurrencies)** — at the emulator with only the endpoint/base-URL changed. The
  unmodified client is the conformance oracle; fork only if unavoidable and document the diff.
- 2026-06-02: **Market prices & quantities use fixed-point ("fast") decimals**, not float64 —
  base-10, 18 fractional digits, signed 128-bit scaled storage, 256-bit mul/div intermediates;
  exact and bit-deterministic. Detailed Go design in PLAN.md §9 (`pkg/decimal`). Confirmed/locked;
  cross-cutting migration from the current float64 prices/volumes is sequenced in PLAN.md §9.8.
- 2026-06-02: Coinbase uses the **Advanced Trade** WS protocol (per-channel subscribe
  `{type:subscribe, channel, product_ids}`; book replies on `l2_data` snapshot/update,
  `new_quantity:"0"` = level removal), **not** the legacy Exchange "channels" array the
  upstream repo used. Confirmed live: both venues stream trades+book; replay is bit-identical.

## Blocked / waiting
- None.

## Next actions
1. Begin Phase 4 return-to-reference: after a user trade perturbs the engine
   book, drain stale synthetics + converge back to reference over `tau`.
2. Carry into Phase 4 (deferred from Phase 3 review): fill accounting — when a
   user trade partially eats a synthetic order, the seeder must top the level
   back to reference size (track desired vs resting remainder separately;
   compare volumes with tolerance, not `==`). Also reconsider holding the
   seeder mutex across many engine calls (snapshot diff, release, then apply)
   so `Clear`/shutdown isn't blocked by a deep reconcile.
