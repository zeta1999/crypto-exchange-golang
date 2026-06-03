package custody

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/txnbuild"
)

// Send signs and submits a Stellar testnet payment of asset ("XLM" native, or
// "USDC") from the wallet secret to destAddr. amount is a 7-dp decimal string.
// Returns the tx hash. Testnet-only (testnet passphrase + testnet Horizon).
func (s *Stellar) Send(ctx context.Context, secret []byte, asset, destAddr, amount string) (string, error) {
	if len(secret) != ed25519.SeedSize {
		return "", fmt.Errorf("xlm: bad seed length %d", len(secret))
	}
	var raw [32]byte
	copy(raw[:], secret)
	kp, err := keypair.FromRawSeed(raw)
	if err != nil {
		return "", fmt.Errorf("xlm: keypair: %w", err)
	}

	var payAsset txnbuild.Asset = txnbuild.NativeAsset{}
	switch asset {
	case "", "XLM":
		// native
	case "USDC":
		payAsset = txnbuild.CreditAsset{Code: "USDC", Issuer: usdcStellarIssuer}
	default:
		return "", ErrUnsupportedAsset
	}

	client := horizonclient.DefaultTestNetClient
	acct, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: kp.Address()})
	if err != nil {
		return "", fmt.Errorf("xlm: load source account (fund it first): %w", err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acct,
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{Destination: destAddr, Amount: amount, Asset: payAsset},
		},
	})
	if err != nil {
		return "", fmt.Errorf("xlm: build payment: %w", err)
	}
	tx, err = tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		return "", fmt.Errorf("xlm: sign payment: %w", err)
	}
	resp, err := client.SubmitTransaction(tx)
	if err != nil {
		return "", fmt.Errorf("xlm: submit payment: %w", horizonError(err))
	}
	return resp.Hash, nil
}

// Received lists incoming payments to address after cursor (exclusive), for
// deposit detection. Only credits TO the address are returned.
func (s *Stellar) Received(ctx context.Context, address, cursor string) ([]Payment, error) {
	client := horizonclient.DefaultTestNetClient
	page, err := client.Payments(horizonclient.OperationRequest{
		ForAccount: address,
		Cursor:     cursor,
		Order:      horizonclient.OrderAsc,
		Limit:      50,
	})
	if err != nil {
		return nil, fmt.Errorf("xlm: payments: %w", horizonError(err))
	}
	var out []Payment
	for _, rec := range page.Embedded.Records {
		p, ok := rec.(operations.Payment)
		if !ok || p.To != address {
			continue // only incoming payment operations
		}
		asset := "XLM"
		if p.Asset.Type != "native" {
			asset = p.Asset.Code
		}
		out = append(out, Payment{
			TxRef:  p.TransactionHash,
			From:   p.From,
			Asset:  asset,
			Amount: p.Amount,
			Cursor: p.PT,
		})
	}
	return out, nil
}
