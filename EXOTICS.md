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

1. **Order entry / exercise for option instruments** (per the options
   architecture notes: `Instrument` → `ContractSpec` with style/settlement/
   multiplier/payoff, then EAPI+venues gain place/cancel).
2. **FIX quote responder:** `NewOrderSingle` for a `payType != VANILLA`
   instrument → look the pricer up in the registry → `Quote` (35=z) back. No other
   layer changes.
3. **Dealer-quoting mode** of `optmarket`: the emulator holds a `Pricer` and
   quotes off the reference book + vol surface, risk-limited — same interface.
4. **Golden-file CI test**: pin a saga-priced scenario the way the scenario
   golden test does, once a dev build of the engine (uncapped, bit-identical RNG)
   is available for CI; until then the tests compare at 1e-3 like the examples
   harness does.
