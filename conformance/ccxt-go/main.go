// CCXT conformance smoke: point a STOCK ccxt-go client at our local
// Binance- or Coinbase-compatible edge by overriding ONLY the base URL, then run
// the normal client lifecycle (loadMarkets → fetchOrderBook → createOrder →
// fetchOpenOrders → cancelOrder). No CCXT source is forked; we mutate
// Urls/Options at runtime.
//
//	go run .            # binance (HMAC)   — needs the edge on :8092
//	go run . coinbase   # coinbase (ES256 JWT) — needs the edge on :8083
package main

import (
	"fmt"
	"os"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

func fail(stage string, err error) {
	fmt.Printf("FAIL [%s]: %v\n", stage, err)
	os.Exit(1)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func flt(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "coinbase" {
		runCoinbase()
		return
	}
	runBinance()
}

func runBinance() {
	base := envOr("BINANCE_URL", "http://localhost:8092/api/v3")

	// Construct WITHOUT credentials so loadMarkets stays purely public — a
	// credentialed client makes CCXT try signed currency discovery against the
	// real venue. Credentials are set after loadMarkets, before signed calls.
	ex := ccxt.NewBinance(map[string]any{"enableRateLimit": false})
	ex.Options.Store("fetchMarkets", []any{"spot"})
	ex.Options.Store("fetchCurrencies", false)

	urls := ex.Urls.(map[string]any)
	api := urls["api"].(map[string]any)
	api["public"] = base
	api["private"] = base
	ex.Urls = urls

	markets, err := ex.LoadMarkets()
	if err != nil {
		fail("loadMarkets", err)
	}
	fmt.Printf("OK  loadMarkets: %d markets\n", len(markets))
	if _, ok := markets["BTC/USD"]; !ok {
		fail("loadMarkets", fmt.Errorf("BTC/USD market not found; got %d keys", len(markets)))
	}
	m := markets["BTC/USD"]
	fmt.Printf("    BTC/USD: id=%v base=%v quote=%v\n", str(m.Id), str(m.BaseCurrency), str(m.QuoteCurrency))

	ex.ApiKey = "conf-key"
	ex.Secret = "conf-secret"
	runLifecycle(ex)
}

func runCoinbase() {
	base := envOr("COINBASE_URL", "http://localhost:8083")
	apiKey := envOr("COINBASE_API_KEY", "conf-key")
	secret := os.Getenv("COINBASE_SECRET") // PEM EC private key (multi-line)
	if f := os.Getenv("COINBASE_SECRET_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			fail("read secret file", err)
		}
		secret = string(b)
	}
	if secret == "" {
		fail("setup", fmt.Errorf("set COINBASE_SECRET or COINBASE_SECRET_FILE to the PEM EC private key"))
	}

	// No credentials for loadMarkets (the /products discovery is public); avoid
	// signed currency discovery.
	ex := ccxt.NewCoinbase(map[string]any{"enableRateLimit": false})
	ex.Options.Store("fetchCurrencies", false)

	// *** Modify ONLY the endpoint URL *** — Coinbase appends /api/v3/brokerage/…
	urls := ex.Urls.(map[string]any)
	api := urls["api"].(map[string]any)
	api["rest"] = base
	ex.Urls = urls

	markets, err := ex.LoadMarkets()
	if err != nil {
		fail("loadMarkets", err)
	}
	fmt.Printf("OK  loadMarkets: %d markets\n", len(markets))
	if _, ok := markets["BTC/USD"]; !ok {
		fail("loadMarkets", fmt.Errorf("BTC/USD market not found; got %d keys", len(markets)))
	}
	m := markets["BTC/USD"]
	fmt.Printf("    BTC/USD: id=%v base=%v quote=%v\n", str(m.Id), str(m.BaseCurrency), str(m.QuoteCurrency))

	// Attach the Advanced Trade ES256 JWT credentials: a PEM secret triggers the
	// JWT path in ccxt-go's coinbase.Sign.
	ex.ApiKey = apiKey
	ex.Secret = secret
	runLifecycle(ex)
}

// exchange is the subset of the ccxt-go client surface the lifecycle uses (both
// *ccxt.Binance and *ccxt.Coinbase satisfy it).
type exchange interface {
	FetchOrderBook(symbol string, options ...ccxt.FetchOrderBookOptions) (ccxt.OrderBook, error)
	CreateLimitOrder(symbol, side string, amount, price float64, options ...ccxt.CreateLimitOrderOptions) (ccxt.Order, error)
	FetchOpenOrders(options ...ccxt.FetchOpenOrdersOptions) ([]ccxt.Order, error)
	CancelOrder(id string, options ...ccxt.CancelOrderOptions) (ccxt.Order, error)
}

func runLifecycle(ex exchange) {
	ob, err := ex.FetchOrderBook("BTC/USD")
	if err != nil {
		fail("fetchOrderBook", err)
	}
	fmt.Printf("OK  fetchOrderBook BTC/USD: %d bids, %d asks", len(ob.Bids), len(ob.Asks))
	if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
		fmt.Printf(" (best bid %.2f / ask %.2f)", ob.Bids[0][0], ob.Asks[0][0])
	}
	fmt.Println()

	order, err := ex.CreateLimitOrder("BTC/USD", "buy", 0.01, 40000)
	if err != nil {
		fail("createLimitOrder", err)
	}
	fmt.Printf("OK  createLimitOrder: id=%v status=%v side=%v price=%.2f amount=%.4f\n",
		str(order.Id), str(order.Status), str(order.Side), flt(order.Price), flt(order.Amount))

	open, err := ex.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol("BTC/USD"))
	if err != nil {
		fail("fetchOpenOrders", err)
	}
	fmt.Printf("OK  fetchOpenOrders BTC/USD: %d open\n", len(open))

	if str(order.Id) != "" {
		canc, err := ex.CancelOrder(str(order.Id), ccxt.WithCancelOrderSymbol("BTC/USD"))
		if err != nil {
			fail("cancelOrder", err)
		}
		fmt.Printf("OK  cancelOrder: id=%v status=%v\n", str(canc.Id), str(canc.Status))
	}

	fmt.Println("\nCCXT-GO CONFORMANCE: PASS")
}
