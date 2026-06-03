# custody — testnet wallet & faucet toolkit

**Testnet only.** A standalone developer / test-bed utility for creating crypto
wallets, funding them from faucets, and checking balances. It is **off by
default and not wired into the exchange server**. Wallet secrets are encrypted
at rest; every chain pins testnet endpoints and a `MustTestnet` guard refuses
any non-testnet network.

## Chains & assets

| Chain | id  | Network          | Native faucet            | Token (faucet)               |
|-------|-----|------------------|--------------------------|------------------------------|
| Stellar | `xlm` | testnet        | friendbot (automatic)    | USDC (trustline + Circle)    |
| Solana  | `sol` | devnet         | airdrop (rate-limited)   | USDC (Circle; ATA auto)      |
| Ethereum| `eth` | sepolia        | manual (web faucet)      | USDC / ERC20 (balance only)  |
| Bitcoin | `btc` | testnet        | manual (web faucet)      | —                            |

Faucets are **tiered**: automated where a programmatic faucet exists
(friendbot, devnet airdrop, Circle via API key); otherwise the tool prints the
web faucet URL + your address and `balance -watch` confirms manual funding.

## Security model

- Secrets (seeds / private keys) are encrypted at rest with **AES-256-GCM**,
  key derived from `CUSTODY_PASSPHRASE` via **PBKDF2-HMAC-SHA256** + a per-file
  random salt. Addresses are stored in the clear; only secrets are encrypted.
- The passphrase is read **only** from the environment, never a flag/argument.
- The keystore file is `0600`, written atomically (fsync + rename).
- The decrypted secret is used only transiently (e.g. to sign a Stellar
  trustline) and zeroed afterward.

## Usage

```sh
export CUSTODY_PASSPHRASE='a strong passphrase'    # required; unlocks/creates the keystore
# export CUSTODY_KEYSTORE=data/custody.keystore.json  # optional path override

custody wallet new -chain xlm -name alice   # create + store an encrypted wallet
custody wallet list                         # name, chain, address
custody wallet address -name alice          # print the deposit address
custody faucet  -name alice                 # tap the testnet faucet (or print a manual URL)
custody balance -name alice [-watch]        # show balances (optionally poll until funded)

# USDC on Stellar needs a trustline first (fund XLM, then prepare, then faucet):
custody faucet  -name alice                 # friendbot funds XLM (for fees/reserve)
custody prepare -name alice -asset USDC     # signs + submits a changeTrust trustline
custody faucet  -name alice -asset USDC     # Circle drip (needs CIRCLE_API_KEY) or web faucet
custody balance -name alice                 # XLM + USDC
```

## Environment

| Var | Purpose |
|-----|---------|
| `CUSTODY_PASSPHRASE` | **required** — unlocks/creates the keystore |
| `CUSTODY_KEYSTORE`   | keystore path (default `data/custody.keystore.json`) |
| `CIRCLE_API_KEY`     | enables automated USDC drips via Circle's faucet API |
| `CIRCLE_SOL_BLOCKCHAIN` / `CIRCLE_XLM_BLOCKCHAIN` | override Circle's blockchain id (defaults `SOL-DEVNET` verified, `XLM-TESTNET` best-guess) |

## Notes

- Solana public **devnet airdrops are heavily rate-limited** and frequently
  return an internal error; retry later.
- The Circle Stellar-testnet blockchain id is unverified (no key to test); if a
  drip is rejected, Circle's error lists the valid value — set
  `CIRCLE_XLM_BLOCKCHAIN` accordingly.
- ERC20/USDC on Ethereum is **balance-read only** here; fund via the Circle /
  Sepolia web faucets.
