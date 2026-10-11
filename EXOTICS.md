# Flow-Exotics Pricing Plugin

Exotics valuation is a **plugin**, not a core dependency: the exchange never names a
concrete pricing engine, everything below the valuation layer keys off one small
interface, and a new backend (another MC engine, in-process Go, a remote service)
is one adapter + one `Register` call.

## Layout

```
internal/pricer/pricer.go    Pricer interface + Params/Result + Registry (the seam)
internal/pricer/saga/        "saga" backend — drives the Sibelius/Aria SibeliusSaga
                             demo binary over its JSON CLI (../price-master-examples)
internal/optmarket/          options book: BS vanilla chain + flow-exotic
                             dealer-quoting mode (Market.AttachPricer)
cmd/examples-pricer/         three-instrument tour (vanilla MC, barrier, arith-asian)
```

## The seam

```go
type Pricer interface {
    Name() string
    Price(ctx context.Context, p Params) (Result, error)  // concurrency-safe
}
```

`Params` carries what any backend might need (spot, vol, rate, T, strike/side,
`num_paths`/`num_steps`/`seed`/`rng`, `model`, payoff `script` or `script_file`,
endpoint override, plus `Raw` map-string for engine-specific extras like a Dupire
vol surface). `Result` echoes `Price`, path counts, RNG and the raw engine output.

Callers depend only on `(name, Params)` — the API edge that answers a FIX
quote-request, the emulator's dealer-quoting mode, a CLI. They hold a `*Registry`
and look up `"saga"` (case-insensitive); nothing else in the repo imports the
backend.

## The "saga" backend

Drives `<root>/bin/SibeliusSaga-linux-amd64` as a subprocess:

```
SibeliusSaga <input.json> <out.json>        # input: {"name":"AriaMCPrice/1","arguments":[{...}]}
```

- **Bit-reproducibility:** every request sets `"rng":"portable"`, so outputs are
  reproducible across OS/compilers — the same guarantee the examples repo uses for
  its goldens, which is exactly why we pin goldens from it (tests compare at the
  harness's own 1e-3 relative tolerance; the shipped demo binary's RNG stream
  differs microscopically from the build that recorded the goldens).
- **Working dir:** `script_file` payoffs resolve relative to the examples checkout
  root, so the adapter sets `cmd.Dir` to the root for the call.
- **Discovery:** root from `SIBELIUS_SAMPLES_ROOT` → checkout-relative auto-find;
  binary from `SAGA_BIN` → `<root>/bin/SibeliusSaga-*` platform probe → sibling
  `Sibelius/build_release`. Overridable per-instance via `Saga{Root, Bin}`.
- **Models:** `bs` (default, `AriaMCPrice`), `dupire`/local-vol (`AriaMCPriceLV`
  with a flat-square surface default or your own under `Raw["surface"]`),
  `hybrid_lv`, plus any endpoint forwarded verbatim (Heston, LSM, PDE…).

## Notes

- **No fabricated vols:** local-vol without an explicit surface requires `Vol > 0`
  (a flat square at that vol is the fallback *surface*, never a fabricated vol).
  Surface precedence: `Params.Raw["surface"]` → `Saga.Surface` → flat square.
- **On engine failure the request payload is preserved** — the error names the
  surviving `input.json`, so a deterministic request can be replayed.
- **Concurrent-safe:** one private temp dir per call; engine stdout/stderr
  captured into bounded per-stream buffers (the binary is known to spew a large
  symbol-table dump on some failures). The `pricer.Registry` tolerates
  `Register` while readers are running.
- **Vanilla without a script:** a `Params{Strike, Side}` with no Script
  generates the minimal European payoff inline (`Saga.PriceVanilla` is the
  explicit form of that).
- First run needs the binary reassembled once in the examples repo:
`cd ../price-master-examples && bin/reassemble.sh linux-amd64`.

## Demo

```
go run ./cmd/examples-pricer
# pricer backend: saga
# European call (BS vanilla payoff, MC)   price =  12.350925   (65536 paths, 176 ms)
# Down-and-out barrier call               price =  13.417530   (65536 paths, 202 ms)
# Arithmetic-average (Asian) call         price =   5.777009   (400000 paths, 1410 ms)
```

All three mirror recorded inputs and goldens from the examples checkout
(`01-vanilla/european-call`, `02-exotics/barrier-down-and-out`,
`02-exotics/asian-arithmetic-bs`).

## Where it plugs in (order of work)

1. **FIX quote responder — DONE (CR-10).** The FIX 4.4 acceptor (`internal/api/fix`)
   now speaks the OTC quote flow end-to-end, wired to this plugin:
   - `QuoteRequest` (35=`z`) {Symbol, Side, OrderQty} → the `Quoter` prices →
     `Quote` (35=`S`) {QuoteID 117, BidPx 132, OfferPx 133, sizes, TransactTime}
   - confirmation: `NewOrderSingle` (35=`D`) with **QuotedOrderFlag=Y (692)**
     and `OrderID` (37) = the QuoteID → executed AT THE QUOTED PRICE (offer for a
     buy, bid for a sell), exactly once, into the same engine book → normal
     ExecutionReport flow (NEW → fills)
   - a **plain D on a quote-only instrument** (no flag) → answered with a fresh
     Quote instead of resting (dealer quoting on inbound interest)
   - reused/expired/foreign-session quotes → Rejected. Quote table is TTL-pruned
     and capped (4096). Config: `api.fix.quotes{enabled,pricer,exotics,vol,
     spread_bps,num_paths,quote_ttl}`; exotics are `api.fix.quotes.exotics[]`
     entries `{engine, payoff | payoff_file, vol?, model?}` (see configs/dev.yaml).
   - `Quoter` is a 2-method interface the fix package owns; `cmd/exchange`
     supplies `fixQuoter`, the adapter that reads the **live engine book mid** as
     spot and prices the exotic's **actual configured payoff** exactly once via
     the registry saga pricer (per-exotic `payoff` inline Aria script or
     `payoff_file`, with optional per-exotic `vol`/`model` overrides). The bid/
     offer bracket that single fair value: `fair ∓ (spread_bps/10000)·fair` — the
     spread is bps of fair value, so the pair is uncrossed by construction and
     always tracks the exotic's own price (a barrier, Asian, autocall, etc. is
     *not* decomposable into vanilla legs — no put-call-parity fabrication).
     Strike/Side are cleared on the Price params so the pricer never falls back
     to a vanilla payoff; a non-positive fair value is an error, not a free
     instrument.
   - Verified: unit (mock quoter: z→S, quoted-D→NEW at quoted price resting on the
     book, double-execute → Rejected, plain-D-on-exotic → S, quoter-failure →
     reject, no-quoter → BusinessReject) + **live smoke** (skip-gated): a real
     server with the saga plugin answered a QuoteRequest, executed the confirmed
     order into the emulator-seeded book, and rejected the reused quote — all
     with real saga prices.

2. **Options dealer-quoting mode — DONE.** The options book (`internal/
   optmarket`) now holds a `Pricer` (via `Market.AttachPricer`) and lists flow
   exotics next to the vanilla Black–Scholes chain, published on the same
   Binance-EAPI surface the vanilla chain already uses:
   - **Vanillas stay Black–Scholes**, untouched. An exotic is a different
     instrument: it carries a bespoke payoff (`.aria` script or file) that has
     no closed form and is **not** decomposable into vanilla legs, so it is
     priced **once, directly** by the registered backend at the underlying
     index — exactly the "price the ACTUAL payoff" discipline the FIX edge uses.
   - **Executable quote:** the single MC fair value is bracketed by the
     configured half-spread (`exotic_spread_bps`, else the market's
     `book_half_spread`) → `bid = fair·(1−w)`, `ask = fair·(1+w)`. Uncrossed by
     construction; a non-finite / non-positive / over-limit fair value is an
     error (negatively cached), never a free instrument.
   - **Provenance, not greeks:** an exotic mark carries `Pricer` + `Model`
     (how the number was produced) and **zero** IV/greeks — an exotic is not a
     Black–Scholes instrument, so no implied vol or greek is invented. The
     EAPI endpoint `GET /eapi/v1/mark/exotic` surfaces that; the vanilla
     `/eapi/v1/mark` stays vanilla-only and rejects an exotic symbol.
   - **Subprocess safety:** a saga call is a subprocess, so each exotic is
     priced at most once per `exotic_ttl` (default 5s) regardless of how often
     marks are read; the vanilla chain is served from the closed form regardless
     of any exotic latency. Config: top-level `options.exotics[]` +
     `options.pricer/exotic_*` knobs (see configs/dev.yaml).
   - A missing saga binary is a **boot warning, not a failure** — the vanilla
     chain keeps serving and exotic marks report no-price until the binary is
     present. Verified with a **live E2E** (skip-gated, offline except the saga
     binary): the real server's exotic mark matched an independently recomputed
     saga price for the same payoff/spot/knobs to MC tolerance, sat below the
     plain vanilla call, was uncrossed, and carried pricer provenance.

3. **Option order entry + exercise/settle for the EAPI option instruments**
   (per the options architecture notes: `Instrument` → `ContractSpec` with
   style/settlement/multiplier/payoff, then place/cancel at EAPI + venues, then an
   expiry sweep settling cash at intrinsic off the index rule).
4. **CI golden** for saga-priced scenarios, once an uncapped, bit-identical engine
   dev build is available (tests compare at 1e-3 rel like the examples harness).
