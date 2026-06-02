# STATUS

_Last updated: 2026-06-02_

## Current phase
**Phase 1 — Feed ingestion** (code complete, CI green, live-verified) → brutal review → next: **Phase 2 — Reference book**

## Legend
☐ not started ◐ in progress ☑ done

## Phase progress
| # | Phase | State | Notes |
|---|-------|-------|-------|
| 0 | Foundations, CI, docs | ☑ | docs + `ci.sh` + Makefile committed; baseline CI **green**; brutal review done + fixes applied |
| 1 | Feed ingestion layer | ◐ | channel-based `feed.Source`; Binance @trade+@depth20, Coinbase market_trades + from-scratch level2; replay+record; `cmd/feedcat`. CI green; **live-verified** both venues + deterministic replay. Pending brutal review. |
| 2 | Reference book | ☐ | snapshot+diff per instrument |
| 3 | Emulator seeding | ☐ | mirror reference liquidity as synthetic orders |
| 4 | Return-to-Reference [a] | ☐ | convergence controller |
| 5 | Trade replay sync | ☐ | inject real tape in sync |
| 6 | Configurable toxicity [b] | ☐ | Kyle λ + VPIN, weighting knobs |
| 7 | Scenario & fault injection (test bed) | ☐ | trace replay, artificial latency, price-shift / arb scenarios |
| 8 | Binance-compatible API | ☐ | REST + WS subset |
| 9 | Coinbase-compatible API | ☐ | Advanced Trade REST + WS subset |
| 10 | Custody examples (stretch) | ☐ | XLM / Solana / ERC20, testnet only |
| 11 | Hardening & observability | ☐ | metrics, scenario tests |

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
- 2026-06-02: Coinbase uses the **Advanced Trade** WS protocol (per-channel subscribe
  `{type:subscribe, channel, product_ids}`; book replies on `l2_data` snapshot/update,
  `new_quantity:"0"` = level removal), **not** the legacy Exchange "channels" array the
  upstream repo used. Confirmed live: both venues stream trades+book; replay is bit-identical.

## Blocked / waiting
- None.

## Next actions
1. Brutal review subagent on Phase 1 feed package; address findings; re-run CI.
2. Begin Phase 2 reference book (consume `feed.Event` stream → per-instrument LOB).
