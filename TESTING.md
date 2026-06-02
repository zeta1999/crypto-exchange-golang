# TESTING — Manual Test Plan

Automated gate is `./ci.sh` (gofmt, vet, lint, build, race tests). This document covers
**manual / scenario testing** run by a human (or a subagent) after each phase. Each phase
section lists prerequisites, steps, and expected results. Record outcomes in `STATUS.md`.

## Conventions
- Run from repo root. Build: `go build ./...`.
- Native dev node: `GOENV=dev go run ./cmd/exchange --config configs/dev.yaml`.
- Use a recorded feed fixture (`testdata/feed/`) for deterministic runs where possible;
  use live WS only for smoke checks (network-dependent).
- "PASS" = expected result observed; note any deviation.

---

## Phase 0 — Foundations
**Steps**
1. `./ci.sh` on a clean checkout.
2. `make ci` (should call `ci.sh`).

**Expected**
- CI runs all stages; exits 0 on the inherited skeleton.
- No formatting diffs reported.

---

## Phase 1 — Feed ingestion
**Prereq:** network access for live; or a recorded fixture for replay.
**Steps**
1. Live smoke: `go run ./cmd/feedcat --venue coinbase --symbol BTC-USD` for ~10s.
2. Live smoke: `go run ./cmd/feedcat --venue binance --symbol BTCUSDT` for ~10s.
3. Record: `go run ./cmd/feedcat --venue coinbase --symbol BTC-USD --record out.jsonl` (~30s).
4. Replay: `go run ./cmd/feedcat --replay out.jsonl`.

**Expected**
- Trades stream with sane price/size/side; book best bid < best ask; spread positive.
- Coinbase `level2` produces a populated book (the feature we added).
- Replay reproduces the recorded sequence identically.

---

## Phase 2 — Reference book
**Steps**
1. Run feed → reference against a fixture; dump `Mid()`, `Spread()`, top 5 levels each side.
2. Inject a known sequence gap in the fixture; observe resync/staleness behavior.

**Expected**
- Reference book matches the raw feed within tolerance at sampled timestamps.
- Gap triggers resync (or staleness flag), no panic, recovers cleanly.

---

## Phase 3 — Emulator seeding
**Steps**
1. Start node with `emulator.enabled: true`, `replay.mode: file`.
2. With **no user orders**, query native snapshot (`GET /snapshot/BTC-USD`) repeatedly.

**Expected**
- Engine book mirrors reference: best bid/ask and depth track the feed each refresh.
- Synthetic orders are tagged distinctly from user orders (visible in logs/WAL).

---

## Phase 4 — Return-to-Reference [a]
**Steps**
1. Seed book from fixture; freeze reference (pause replay).
2. Submit a user market order that consumes 2–3 levels; snapshot immediately.
3. Resume; snapshot every 500ms for `2*tau`.

**Expected**
- Immediately after the trade the book shows the dent.
- Over ~`tau` the book converges back to reference; stale synthetics drained first.
- Deterministic across two runs with the same seed.

---

## Phase 5 — Trade replay sync
**Steps**
1. Place a resting user limit BUY just below current mid.
2. Replay a session where the tape prints down through that price.

**Expected**
- The user limit fills at/around the tape time and price (not instantly, not never).
- Fill price/size consistent with the printed trade; logged with tape correlation.

---

## Phase 6 — Configurable toxicity [b]
**Steps**
1. Run scenario with `toxicity.scale: 0` — place resting user limits, replay session.
2. Repeat with `toxicity.scale: 2.0` (over-weight), same seed/session.
3. Compare adverse-fill counts and fill prices; check λ/VPIN logged gauges.

**Expected**
- `scale: 0` ⇒ behavior reduces to pure RTR (no extra adverse selection).
- Higher scale ⇒ resting user limits get picked off more often and nearer unfavorable
  prints; λ/VPIN values move with tape activity.
- Results reproducible with fixed `seed`.

---

## Phase 7 — Binance-compatible API
**Prereq:** `api.binance.enabled: true`.
**Steps**
1. `GET /api/v3/depth?symbol=BTCUSDT` via curl.
2. Signed `POST /api/v3/order` (LIMIT BUY) using a test HMAC key; then `DELETE`.
3. `GET /api/v3/openOrders`, `GET /api/v3/account`.
4. Connect a WS client to `@depth`/`@trade` and the user-data stream.

**Expected**
- Responses match Binance JSON shapes; signature check enforced.
- Order appears in openOrders; cancel removes it; executionReport pushed on fill.
- (Bonus) `python-binance` against the emulator base URL works for the above.

---

## Phase 8 — Coinbase-compatible API
**Prereq:** `api.coinbase.enabled: true`.
**Steps**
1. `GET` product book + ticker via curl.
2. Signed create-order + cancel-order (JWT/HMAC).
3. `GET` list orders.
4. WS subscribe `level2`, `market_trades`, `user`.

**Expected**
- Responses match Advanced Trade shapes; auth enforced.
- Order lifecycle reflected in list orders + user channel.

---

## Phase 9 — Custody examples (stretch, testnet only)
**Prereq:** custody flag on; testnet RPC endpoints + funded faucet keys in env.
**Steps**
1. Generate a deposit address (XLM / Solana / ERC20-Sepolia).
2. Send a small testnet deposit; wait for confirmation.
3. Verify balance credited; place a trade funded by it.
4. Request a small withdrawal; confirm a testnet tx broadcasts.

**Expected**
- Deposit credits balance after confirmations; withdrawal produces a valid testnet txid.
- No mainnet calls; keys never logged.

---

## Phase 10 — Hardening & observability
**Steps**
1. Scrape `/metrics`; confirm book-deviation, λ/VPIN, fill, feed-lag series exist.
2. Run the scenario test suite (`go test ./... -run Scenario`).
3. Feed a malformed config; confirm validation error (no panic).

**Expected**
- Metrics populated and sane; scenario goldens pass; bad config rejected clearly.

---

## Regression smoke (run any time)
1. `./ci.sh` green.
2. Native node starts, places + cancels an order over HTTP, streams a snapshot over gRPC/WS.
3. Emulator on a fixture runs 60s with no panics; book stays near reference.
