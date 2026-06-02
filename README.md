<p align="center">
  <img src="image/logo.svg" width="180" alt="mirage">
</p>

<h1 align="center">mirage</h1>

<p align="center">
  <a href="#"><img src="https://img.shields.io/badge/status-WIP%20·%20phase%200-ff9d2b?style=flat-square" alt="status"></a>
  <a href="#"><img src="https://img.shields.io/badge/go-1.25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="go"></a>
  <a href="#"><img src="https://img.shields.io/badge/matching-price--time-2ed573?style=flat-square" alt="matching"></a>
  <a href="#"><img src="https://img.shields.io/badge/APIs-Binance%20%7C%20Coinbase-6c5ce7?style=flat-square" alt="apis"></a>
  <a href="#"><img src="https://img.shields.io/badge/transports-gRPC%20%7C%20HTTP%20%7C%20WS-00ADD8?style=flat-square" alt="transports"></a>
  <a href="#"><img src="https://img.shields.io/badge/CI-local%20ci.sh-2ed573?style=flat-square" alt="ci"></a>
</p>

<p align="center">
  <sub>codename — easily renamed in <code>go.mod</code> / README</sub>
</p>

<p align="center">
  <b>A configurable crypto-exchange emulator with a real matching engine.</b><br>
  Mirrors live Coinbase / Binance markets in real time, speaks <b>Binance- and
  Coinbase-compatible APIs</b>, and is built to be a <b>test bed for trading & OMS
  systems</b> — deterministic, scriptable, and adversarial on demand.
</p>

---

> **WIP — Phase 0.** Plan and local CI are in place; the emulator core lands across
> the phases in [PLAN.md](PLAN.md). Track progress in [STATUS.md](STATUS.md),
> the checklist in [TODO.md](TODO.md), and the manual test plan in [TESTING.md](TESTING.md).

## Why

You want to test an OMS or a trading strategy against something that behaves like a real
venue — without paying for one, without rate limits, and **with the ability to reproduce a
scenario exactly**. `mirage` anchors an internal limit order book to a **reference book**
rebuilt live from a real exchange feed, runs a genuine price-time matching engine on top,
and lets you bend reality with config: inject latency, shift prices, replay a recorded
session, or crank up market toxicity.

## What's Inside

### The two core mechanisms

| | Mechanism | What it does |
|---|-----------|--------------|
| **[a]** | **Return-to-Reference (RTR)** | After your order perturbs the book, the emulator *progressively converges* the local LOB back toward the live feed — draining stale synthetic liquidity first, then tracking the spot as it moves. Convergence speed is a knob (`tau`). |
| **[b]** | **Configurable toxicity** | Resting limit orders get adversely selected per a tunable parameter, driven by **tape-induced models** — **Kyle's λ** (price impact) and **VPIN** (informed-trading proxy) — with **over/under-weight knobs** (`scale: 0` = off). |

### Test-bed controls

Purpose-built for technical and scenario testing of trading / OMS systems:

| Control | Use |
|---------|-----|
| **Trace replay** | Replay past market events from recorded traces — deterministic, repeatable scenario runs at real-time or accelerated `speed`. |
| **Artificial latency** | Inject configurable delays (feed→book, order ack, fill report, per-API-edge) to test how your OMS handles slow venues and races. |
| **Artificial price shift** | Offset / scale the reference price per venue to manufacture **cross-venue dislocations** — a controlled lab for **arbitrage** and relative-value models. |
| **Seedable determinism** | Matching is deterministic; RTR + toxicity use a seeded RNG, so a scenario reproduces bit-for-bit. |

### API surface

| Surface | Shape | Status |
|---------|-------|--------|
| **Binance-compatible** | REST `/api/v3/*` (order, depth, ticker, account) + WS streams + HMAC-SHA256 signing | planned (Phase 8) |
| **Coinbase-compatible** | Advanced Trade REST + WS (`level2`, `market_trades`, `user`) + JWT/HMAC | planned (Phase 9) |
| **Native** | gRPC bidi-stream, HTTP REST, WebSocket | working today |

> API compatibility ships a **documented subset**, not 100% parity — enough to point an
> existing client/bot at the emulator.

### Live data feeds

Real-time order books and trades, vendored & normalized from the
[`this-is-not-bbg`](../this-is-not-bbg) adapters:

| Venue | Streams |
|-------|---------|
| **Binance** | `@depth20@100ms` (book), `@trade` |
| **Coinbase** | `market_trades`, `level2` (book) |
| **Replay** | file-backed source — recorded traces, fully deterministic |

### Custody examples (stretch, testnet only)

Optional, off by default: deposit / balance / withdraw demos on **XLM** (Horizon testnet),
**Solana** (devnet), and **ERC20** (Sepolia). Keys via env, never mainnet.

## Architecture

```
  Real venue WS ─► feed/ (Binance, Coinbase, replay — normalized)
                     │  reference LOB + real trade tape
                     ▼
                 emulator/ ── Seeder ──► mirror reference as synthetic liquidity
                     │  ├─ [a] RTR controller     (converge book → reference)
                     │  ├─ [b] toxicity model     (Kyle λ / VPIN adverse-select)
                     │  ├─ replay + clock          (real-time / accelerated)
                     │  └─ fault injection         (latency · price-shift)
                     ▼
              internal/engine + internal/orderbook   (real price-time matching)
                     ▼
        api/binance · api/coinbase · native gRPC/HTTP/WS · custody/ (optional)
                     ▲
              your OMS / trading strategy under test
```

## Quick Start

```bash
# build + full local CI gate (gofmt, vet, lint, build, race tests)
make ci

# run the native exchange node against the dev config
make run            # GOENV=dev go run ./cmd/exchange --config configs/dev.yaml

# (Phase 1+) inspect a live or recorded feed
make feedcat
```

> CI is a **local `./ci.sh`** script (not GitHub Actions). Run it before every commit.

## Configuration

Behavior is config-driven — retune without recompiling (`configs/dev.yaml`):

```yaml
emulator:
  enabled: true
  venue: coinbase            # coinbase | binance
  instruments: ["BTC-USD", "ETH-USD"]
  rtr:        { tau_ms: 3000, drain_stale_first: true }   # [a]
  toxicity:   { scale: 1.0, kyle_weight: 1.0, vpin_weight: 1.0, window_trades: 500, seed: 42 }   # [b]
  replay:     { mode: live, file: "", speed: 1.0 }        # live | file (trace replay)
  latency:    { feed_to_book_ms: 0, order_ack_ms: 0, fill_report_ms: 0, jitter_ms: 0 }
  price_shift: { offset_bps: 0, scale: 1.0 }              # manufacture cross-venue arb
api:
  binance:  { enabled: false, listen: ":8082" }
  coinbase: { enabled: false, listen: ":8083" }
```

## Repository Layout

```
cmd/exchange/        runnable node (matching engine + API servers)
cmd/feedcat/         feed inspector (Phase 1)
internal/engine/     order validation + routing
internal/orderbook/  price-time matching, per-instrument books
internal/feed/        vendored Binance/Coinbase WS adapters + replay (Phase 1)
internal/reference/  live reference book (Phase 2)
internal/emulator/   seeder · RTR · replay · fault injection (Phase 3–7)
internal/toxicity/   Kyle λ + VPIN estimators (Phase 6)
internal/api/        gRPC / HTTP / WS native + binance/ + coinbase/ adapters
internal/margin/     risk validation (stub)
pkg/{wal,config,auth} durability, config, token auth
proto/exchange/v1/   gRPC service definitions
```

## Non-Goals

- A production exchange (settlement finality, full regulatory surface).
- Mainnet custody / real funds.
- 100% API coverage of either venue.

## License

TBD.

---

<p align="center"><sub>Built as a deterministic, adversarial sandbox for trading & OMS development.</sub></p>
