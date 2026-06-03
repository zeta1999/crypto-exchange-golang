package custody

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
)

// PrepareAsset establishes a Stellar trustline so the account can hold the
// asset (currently USDC). The account must already exist and hold enough XLM
// for the trustline's base reserve + fee — run `faucet` (friendbot) first. It
// builds, signs (ed25519, testnet network), and submits a changeTrust tx via
// testnet Horizon, returning the tx hash.
//
// This is the only signing path in the toolkit; it is testnet-only (the
// testnet network passphrase + testnet Horizon are hardcoded).
func (s *Stellar) PrepareAsset(ctx context.Context, secret []byte, asset string) (string, error) {
	if asset != "USDC" {
		return "", ErrUnsupportedAsset
	}
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("xlm: bad seed length %d", len(secret))
	}
	var raw [32]byte
	copy(raw[:], secret)
	kp, err := keypair.FromRawSeed(raw)
	if err != nil {
		return "", fmt.Errorf("xlm: keypair: %w", err)
	}

	client := horizonclient.DefaultTestNetClient
	acct, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: kp.Address()})
	if err != nil {
		return "", fmt.Errorf("xlm: load account (fund it via faucet first): %w", err)
	}

	line := txnbuild.CreditAsset{Code: asset, Issuer: usdcStellarIssuer}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acct,
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{
			&txnbuild.ChangeTrust{Line: line.MustToChangeTrustAsset()},
		},
	})
	if err != nil {
		return "", fmt.Errorf("xlm: build trustline tx: %w", err)
	}
	tx, err = tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		return "", fmt.Errorf("xlm: sign trustline tx: %w", err)
	}
	resp, err := client.SubmitTransaction(tx)
	if err != nil {
		return "", fmt.Errorf("xlm: submit trustline tx: %w", horizonError(err))
	}
	return resp.Hash, nil
}

// horizonError unwraps a Horizon Problem into a readable message (Horizon
// returns the failure reason in result_codes within a structured error).
func horizonError(err error) error {
	if p := horizonclient.GetError(err); p != nil {
		if rc, rerr := p.ResultCodes(); rerr == nil {
			return fmt.Errorf("%s (tx=%s ops=%v)", p.Problem.Title, rc.TransactionCode, rc.OperationCodes)
		}
		return fmt.Errorf("%s: %s", p.Problem.Title, p.Problem.Detail)
	}
	return err
}
