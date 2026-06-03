# CCXT-Go conformance smoke

Points the **stock** [`ccxt-go`](https://pkg.go.dev/github.com/ccxt/ccxt/go/v4)
client at our compatible edge — changing **only the base URL** — and runs the
normal client lifecycle:

```
loadMarkets → fetchOrderBook → createLimitOrder → fetchOpenOrders → cancelOrder
```

Two modes (one binary):

```sh
go run .            # binance  (HMAC-SHA256)  — edge on :8092
go run . coinbase   # coinbase (Advanced Trade, ES256 JWT) — edge on :8083
```

If an unmodified exchange client drives the edge end-to-end, the edge is
protocol-conformant for that surface. CCXT is the oracle; we fork nothing.

## Why a separate module

This directory has its **own `go.mod`** so the CCXT dependency tree
(go-ethereum, blst, …) never enters the main module or its CI. `go build ./...`
and `ci.sh` at the repo root skip nested modules.

## How it overrides only the endpoint

No CCXT source is patched. At runtime the harness:

1. constructs `ccxt.NewBinance` **without credentials** (so `loadMarkets` stays
   purely public — a credentialed client makes CCXT attempt signed currency
   discovery against the real venue),
2. sets `Options["fetchMarkets"] = ["spot"]` and `Options["fetchCurrencies"] =
   false` (our edge serves the `/api/v3` spot subset, no `sapi`/`fapi`),
3. rewrites `Urls["api"]["public"]` and `["private"]` to the local edge,
4. attaches credentials *after* `loadMarkets`, before the signed calls.

## Run it

```sh
# 1. Boot the edge offline (replay venue, plain HTTP, binance edge on :8092).
#    See configs/dev.yaml; set api.binance.enabled: true, api_key/secret, and
#    network.tls.*: "" for plain HTTP. Example config in the PR that added this.
EXCHANGE_CONFIG=/path/to/smoke.yaml go run ./cmd/exchange

# 2. In another shell:
cd conformance/ccxt-go
go run .
```

Expected:

```
OK  loadMarkets: 2 markets
    BTC/USD: id=BTCUSDT base=BTC quote=USD
OK  fetchOrderBook BTC/USD: 1 bids, 2 asks (best bid 41998.50 / ask 42001.00)
OK  createLimitOrder: id=2 status=open side=buy price=40000.00 amount=0.0100
OK  fetchOpenOrders BTC/USD: 1 open
OK  cancelOrder: id=2 status=canceled

CCXT-GO CONFORMANCE: PASS
```

## Conformance bugs this surfaced (now fixed)

Running the real client found two genuine gaps in the SIGNED-request path that
the in-repo query-string tests missed — real Binance clients send a SIGNED
**POST**'s params *and* signature in a form-urlencoded **body** with an empty
query string:

1. **Signature** must be computed over Binance's `totalParams = queryString +
   body`, not `r.URL.RawQuery` alone (was `-1022`).
2. **Timestamp** must be read from the parsed form (query + body), not the query
   only (was `-1102`).

Both fixed in `internal/api/binance/auth.go` with regression tests in
`auth_test.go` (`TestVerify_BodySignedPOST`, `TestVerify_BodyTamperedRejected`).

## Coinbase (ES256 JWT) — `go run . coinbase`

`ccxt.NewCoinbase` is the Advanced Trade class; it signs with **ES256 JWT** when
the secret is a PEM EC private key. The harness:

1. constructs `ccxt.NewCoinbase` **without credentials** (public `loadMarkets`),
   `Options["fetchCurrencies"] = false`,
2. rewrites `Urls["api"]["rest"]` to the local edge (the only change),
3. after `loadMarkets`, sets `ApiKey` = key name and `Secret` = the PEM EC
   private key, which triggers ccxt's JWT path for the signed calls.

Generate a key, boot the edge with the **public** PEM as
`coinbase.jwt_public_key`, then:

```sh
openssl ecparam -genkey -name prime256v1 -noout -out /tmp/cb-priv.pem
openssl ec -in /tmp/cb-priv.pem -pubout -out /tmp/cb-pub.pem
COINBASE_URL=http://localhost:8083 COINBASE_API_KEY=test-key \
  COINBASE_SECRET_FILE=/tmp/cb-priv.pem go run . coinbase
rm -f /tmp/cb-priv.pem /tmp/cb-pub.pem   # the private key is a secret
```

Verified `CCXT-GO CONFORMANCE: PASS`. ccxt-go (>= v4.5) discovers markets via the
public `brokerage/market/products` and reads depth via `brokerage/market/product_book`;
the edge aliases both to the legacy routes (`internal/api/coinbase`, `TestMarketAliasRoutes`).
See EXTRA-TESTING.md for the full runbook + the 401 negative checks.
