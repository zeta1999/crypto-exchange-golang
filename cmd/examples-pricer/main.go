// Command examples-pricer is a tiny tour of the flow-exotics pricing plugin:
// it registers the Saga (Sibelius/Aria) backend in a pricer.Registry and
// prices three simple books — all from the sibling
// ../price-master-examples checkout, using the repo's own inputs verbatim.
//
//	Run:
//	  go run ./cmd/examples-pricer
//
//	Override the checkout/binary with SIBELIUS_SAMPLES_ROOT / SAGA_BIN if it
//	lives somewhere else; the binary must first be reassembled there
//	(cd ../price-master-examples && bin/reassemble.sh).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer/saga"
)

func main() {
	reg := pricer.NewRegistry()
	reg.Register(&saga.Saga{
		// The Asian job runs 400k paths; the 2-min default is too small for
		// slow machines — the outer ctx here is 10 min, match it per-call.
		Timeout: 10 * time.Minute,
	}) // Root/Bin empty → auto-discovered

	engine, ok := reg.Get("saga")
	if !ok {
		log.Fatal("saga backend not registered (auto-discovery failed: no price-master-examples checkout?)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	type job struct {
		name   string
		params pricer.Params
	}
	jobs := []job{
		{
			name: "European call (BS vanilla payoff, MC)     [examples/01-vanilla]",
			params: pricer.Params{
				Script: `contract Call { let S = spot("X") one(KRW) |> scale(max(S - 100.0, 0.0)) |> when(2026-01-01) }`,
				Spot:   100, Vol: 0.25, Rate: 0.05, ExpiryYears: 1.0,
				NumPaths: 65536, NumSteps: 252, Seed: 42,
			},
		},
		{
			name: "Down-and-out barrier call                [examples/02-exotics]",
			params: pricer.Params{
				Script: `contract DownAndOutCall { let S = spot("X") var alive = 1.0 alive := alive * cond(S > 80.0, 1.0, 0.0) one(KRW) |> scale(alive * max(S - 100.0, 0.0)) |> when(2026-01-01) }`,
				Spot:   100, Vol: 0.30, Rate: 0.05, ExpiryYears: 1.0,
				NumPaths: 65536, NumSteps: 252, Seed: 42,
			},
		},
		{
			name: "Arithmetic-average (Asian) call          [examples/02-exotics]",
			params: pricer.Params{
				ScriptFile: "payoffs/asian_arith_call.aria",
				Spot:       100, Vol: 0.20, Rate: 0.05, ExpiryYears: 1.0,
				NumPaths: 400000, NumSteps: 252, Seed: 42,
			},
		},
	}

	fmt.Printf("pricer backend: %s\n", engine.Name())
	for _, j := range jobs {
		res, err := engine.Price(ctx, j.params)
		if err != nil {
			log.Printf("%-50s ERROR %v", j.name, err)
			os.Exit(1)
		}
		fmt.Printf("%-50s price = %12.6f   (%d paths, %.0f ms, rng=%s)\n",
			j.name, res.Price, res.NumPaths, res.ElapsedMS, res.RNG)
	}
}
