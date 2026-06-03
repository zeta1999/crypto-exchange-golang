// Command custody is a TESTNET-ONLY tool to create encrypted crypto wallets and
// fund them from faucets. It is a standalone developer / test-bed utility and is
// NOT part of the exchange server. Wallet secrets are encrypted at rest in a
// keystore unlocked by the CUSTODY_PASSPHRASE environment variable.
//
// Usage:
//
//	custody wallet new -chain xlm -name alice   # create + store an encrypted wallet
//	custody wallet list                         # list wallets (name, chain, address)
//	custody wallet address -name alice          # print a wallet's deposit address
//	custody faucet -name alice                  # tap the testnet faucet
//	custody balance -name alice [-watch]        # show balances (optionally poll)
//
// Environment:
//
//	CUSTODY_PASSPHRASE  (required) unlocks/creates the keystore
//	CUSTODY_KEYSTORE    keystore path (default: data/custody.keystore.json)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/custody"
)

const usage = `custody — TESTNET-ONLY wallet & faucet tool

commands:
  wallet new -chain <id> -name <name>   create + store an encrypted wallet
  wallet list                           list wallets
  wallet address -name <name>           print a wallet's deposit address
  prepare -name <name> -asset <A>       enable holding a token (e.g. USDC trustline)
  faucet -name <name> [-asset A] [-amount F]   tap the testnet faucet
  balance -name <name> [-watch]         show balances (optionally poll)

chains: xlm (Stellar testnet), sol (Solana devnet), eth (Ethereum Sepolia)
USDC: on xlm, run "prepare -asset USDC" (establishes a trustline; fund XLM
      first) before "faucet -asset USDC". Circle USDC drip needs CIRCLE_API_KEY;
      without it, faucet prints the web faucet URL.

env:
  CUSTODY_PASSPHRASE  (required) unlocks/creates the keystore
  CUSTODY_KEYSTORE    keystore path (default: data/custody.keystore.json)
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	reg := custody.NewRegistry()
	reg.Register(custody.NewStellar())
	reg.Register(custody.NewSolana())
	reg.Register(custody.NewEVM())

	var err error
	switch os.Args[1] {
	case "wallet":
		err = cmdWallet(reg, os.Args[2:])
	case "prepare":
		err = cmdPrepare(reg, os.Args[2:])
	case "faucet":
		err = cmdFaucet(reg, os.Args[2:])
	case "balance":
		err = cmdBalance(reg, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		err = fmt.Errorf("unknown command %q (try: custody help)", os.Args[1])
	}
	if err != nil {
		log.Fatalf("custody: %v", err)
	}
}

func openKeystore() (*custody.Keystore, error) {
	path := os.Getenv("CUSTODY_KEYSTORE")
	if path == "" {
		path = "data/custody.keystore.json"
	}
	pass := os.Getenv("CUSTODY_PASSPHRASE")
	if pass == "" {
		return nil, errors.New("CUSTODY_PASSPHRASE is required")
	}
	return custody.Open(path, pass)
}

func cmdWallet(reg *custody.Registry, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: custody wallet <new|list|address> ...")
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("wallet new", flag.ExitOnError)
		chain := fs.String("chain", "", "chain id (xlm|sol)")
		name := fs.String("name", "", "wallet name")
		_ = fs.Parse(args[1:])
		if *chain == "" || *name == "" {
			return errors.New("wallet new requires -chain and -name")
		}
		ch, err := reg.Get(*chain)
		if err != nil {
			return err
		}
		ks, err := openKeystore()
		if err != nil {
			return err
		}
		secret, err := ch.NewKey()
		if err != nil {
			return err
		}
		addr, err := ch.Address(secret)
		if err != nil {
			return err
		}
		if err := ks.Put(*name, ch.ID(), addr, secret); err != nil {
			return err
		}
		fmt.Printf("created %s wallet %q\n  network: %s\n  address: %s\n", ch.ID(), *name, ch.Network(), addr)
		return nil

	case "list":
		ks, err := openKeystore()
		if err != nil {
			return err
		}
		entries := ks.List()
		if len(entries) == 0 {
			fmt.Println("(no wallets)")
			return nil
		}
		for _, e := range entries {
			fmt.Printf("%-16s %-4s %s\n", e.Name, e.Chain, e.Address)
		}
		return nil

	case "address":
		fs := flag.NewFlagSet("wallet address", flag.ExitOnError)
		name := fs.String("name", "", "wallet name")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return errors.New("wallet address requires -name")
		}
		ks, err := openKeystore()
		if err != nil {
			return err
		}
		_, addr, ok := ks.Lookup(*name)
		if !ok {
			return fmt.Errorf("no wallet %q", *name)
		}
		fmt.Println(addr)
		return nil

	default:
		return fmt.Errorf("unknown wallet subcommand %q", args[0])
	}
}

func cmdPrepare(reg *custody.Registry, args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	name := fs.String("name", "", "wallet name")
	asset := fs.String("asset", "", "asset to enable (e.g. USDC)")
	_ = fs.Parse(args)
	if *name == "" || *asset == "" {
		return errors.New("prepare requires -name and -asset")
	}
	ks, err := openKeystore()
	if err != nil {
		return err
	}
	chainID, _, secret, err := ks.Get(*name) // decrypt the secret only for signing
	if err != nil {
		return err
	}
	ch, err := reg.Get(chainID)
	if err != nil {
		return err
	}
	prep, ok := ch.(custody.TokenPreparer)
	if !ok {
		return fmt.Errorf("chain %s does not require asset preparation", chainID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ref, err := prep.PrepareAsset(ctx, secret, *asset)
	zero(secret) // best-effort wipe of the decrypted seed once signing is done
	if err != nil {
		return err
	}
	fmt.Printf("prepared %s for %s\n  ref: %s\n", *asset, *name, ref)
	return nil
}

// zero overwrites a secret slice (best-effort; Go's GC may already have copied).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func cmdFaucet(reg *custody.Registry, args []string) error {
	fs := flag.NewFlagSet("faucet", flag.ExitOnError)
	name := fs.String("name", "", "wallet name")
	asset := fs.String("asset", "", "asset to fund (default: native)")
	amount := fs.Float64("amount", 0, "amount (chain default if 0)")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("faucet requires -name")
	}
	ks, err := openKeystore()
	if err != nil {
		return err
	}
	chainID, addr, ok := ks.Lookup(*name)
	if !ok {
		return fmt.Errorf("no wallet %q", *name)
	}
	ch, err := reg.Get(chainID)
	if err != nil {
		return err
	}
	f, ok := ch.(custody.Faucet)
	if !ok {
		return custody.ErrNoFaucet
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ref, err := f.Tap(ctx, addr, *asset, *amount)
	if err != nil {
		// On a manual faucet, OR any automated-faucet failure when a web faucet
		// exists, point the user at the web faucet + their address.
		if url, ok := f.ManualURL(*asset); ok {
			if errors.Is(err, custody.ErrManualFaucet) {
				fmt.Printf("manual faucet — visit:\n  %s\nfund address: %s\n", url, addr)
			} else {
				fmt.Printf("automated faucet failed (%v)\ntry the web faucet:\n  %s\nfund address: %s\n", err, url, addr)
			}
			return nil
		}
		return err
	}
	fmt.Printf("tapped %s for %s\n  ref: %s\n", chainID, addr, ref)
	return nil
}

func cmdBalance(reg *custody.Registry, args []string) error {
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	name := fs.String("name", "", "wallet name")
	watch := fs.Bool("watch", false, "poll until a balance appears")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("balance requires -name")
	}
	ks, err := openKeystore()
	if err != nil {
		return err
	}
	chainID, addr, ok := ks.Lookup(*name)
	if !ok {
		return fmt.Errorf("no wallet %q", *name)
	}
	ch, err := reg.Get(chainID)
	if err != nil {
		return err
	}

	const maxPolls = 30
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		bals, err := ch.Balances(ctx, addr)
		cancel()
		if err != nil {
			return err
		}
		if len(bals) > 0 || !*watch || attempt >= maxPolls {
			if len(bals) == 0 {
				fmt.Printf("%s %s: (no balances — unfunded)\n", chainID, addr)
				return nil
			}
			fmt.Printf("%s %s:\n", chainID, addr)
			for _, b := range bals {
				fmt.Printf("  %-6s %s\n", b.Asset, b.Amount)
			}
			return nil
		}
		fmt.Printf("waiting for funds… (%d/%d)\n", attempt+1, maxPolls)
		time.Sleep(3 * time.Second)
	}
}
