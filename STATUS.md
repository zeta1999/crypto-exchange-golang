# STATUS

_Last updated: 2026-06-02_

## Current phase
**Runnable product + exact decimals** ✅ — `go run ./cmd/exchange` boots a live Coinbase-mirroring
exchange (feed → reference → seeded synthetic liquidity + RTR), tradable via gRPC/HTTP/WS, with
all prices/quantities now exact `decimal.Decimal` (matching core + reference + emulator migrated;
API edges convert; feed stays float64). Verified live (book 20 levels/side, uncrossed; HTTP emits
exact decimal strings). Phases 1–5, `pkg/decimal`, and the float64→Decimal migration done & reviewed.
Phase 5 (trade replay) injects the live tape via a single-lock IOC primitive so
resting user orders fill in sync. **Next:** Phase 6 — configurable toxicity;
then 7–11.

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
| 6 | Configurable toxicity [b] | ☐ | Kyle λ + VPIN, weighting knobs |
| 7 | Scenario & fault injection (test bed) | ☐ | trace replay, artificial latency, price-shift / arb scenarios |
| 8 | Binance-compatible API | ☐ | REST + WS subset |
| 9 | Coinbase-compatible API | ☐ | Advanced Trade REST + WS subset |
| 10 | Custody examples (stretch) | ☐ | XLM / Solana / ERC20, testnet only |
| 11 | Hardening & observability | ☐ | metrics, scenario tests |

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
