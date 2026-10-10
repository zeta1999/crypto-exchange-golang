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

## Where it plugs in next (order of work)

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
     spread_bps,num_paths,quote_ttl}` (see configs/dev.yaml).
   - `Quoter` is a 2-method interface the fix package owns; `cmd/exchange`
     supplies `fixQuoter`, the adapter that reads the **live engine book mid** as
     spot, prices an ATM European via the registry saga pricer (C + P legs), and
     quotes `S−K+put / S−K+call` (put-call parity ⇒ uncrossed) ± half spread.
   - Verified: unit (mock quoter: z→S, quoted-D→NEW at quoted price resting on the
     book, double-execute → Rejected, plain-D-on-exotic → S, quoter-failure →
     reject, no-quoter → BusinessReject) + **live smoke** (`TestManualSmoke`,
     skip-gated): a real server with the saga plugin answered a QuoteRequest,
     executed the confirmed order into the emulator-seeded book, and rejected the
     reused quote — all with real saga prices.

2. **Option order entry + exercise/settle for the EAPI option instruments**
   (per the options architecture notes: `Instrument` → `ContractSpec` with
   style/settlement/multiplier/payoff, then place/cancel at EAPI + venues, then an
   expiry sweep settling cash at intrinsic off the index rule).
3. **Dealer-quoting mode of `optmarket`**: the emulator holds a `Pricer` and quotes
   exotic books off the reference book + vol surface under risk limits — same
   `Quoter` seam the FIX edge now uses.
4. **CI golden** for saga-priced scenarios, once an uncapped, bit-identical engine
   dev build is available (tests compare at 1e-3 rel like the examples harness).
