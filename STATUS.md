# STATUS

_Last updated: 2026-06-02_

## Current phase
**Phase 0 — Foundations, CI, docs** (complete) → next: **Phase 1 — Feed ingestion**

## Legend
☐ not started ◐ in progress ☑ done

## Phase progress
| # | Phase | State | Notes |
|---|-------|-------|-------|
| 0 | Foundations, CI, docs | ☑ | docs + `ci.sh` + Makefile committed; baseline CI **green**; brutal review done + fixes applied |
| 1 | Feed ingestion layer | ☐ | Port Binance/Coinbase WS adapters from `../this-is-not-bbg` |
| 2 | Reference book | ☐ | snapshot+diff per instrument |
| 3 | Emulator seeding | ☐ | mirror reference liquidity as synthetic orders |
| 4 | Return-to-Reference [a] | ☐ | convergence controller |
| 5 | Trade replay sync | ☐ | inject real tape in sync |
| 6 | Configurable toxicity [b] | ☐ | Kyle λ + VPIN, weighting knobs |
| 7 | Binance-compatible API | ☐ | REST + WS subset |
| 8 | Coinbase-compatible API | ☐ | Advanced Trade REST + WS subset |
| 9 | Custody examples (stretch) | ☐ | XLM / Solana / ERC20, testnet only |
| 10 | Hardening & observability | ☐ | metrics, scenario tests |

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

## Blocked / waiting
- None.

## Next actions
1. Finish Phase 0: add `ci.sh`, `Makefile`; run baseline CI.
2. Commit Phase 0 (no push).
3. Brutal review subagent on Phase 0 scaffolding; address findings.
4. Begin Phase 1 feed ingestion.
