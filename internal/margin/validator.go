package margin

import (
	"context"
	"fmt"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// Check defines the signature for user supplied margin call logic.
type Check func(ctx context.Context, ord *orderbook.Order, snap *orderbook.Snapshot) error

// Validator executes a list of checks per order so risk owners can plug custom logic.
type Validator struct {
	book   SnapshotProvider
	checks []Check
}

// SnapshotProvider exposes the minimal book data margin needs.
type SnapshotProvider interface {
	Snapshot(symbol string) (*orderbook.Snapshot, error)
}

// NewValidator creates a validator storing user provided checks.
func NewValidator(book SnapshotProvider, checks ...Check) *Validator {
	return &Validator{book: book, checks: checks}
}

// Validate executes each registered check sequentially.
func (v *Validator) Validate(ctx context.Context, ord *orderbook.Order) error {
	snap, err := v.book.Snapshot(ord.Instrument)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	for _, check := range v.checks {
		if err := check(ctx, ord, snap); err != nil {
			return err
		}
	}
	return nil
}

// WithNotionalLimit is an example check to keep notional below a threshold.
func WithNotionalLimit(limit float64) Check {
	return func(ctx context.Context, ord *orderbook.Order, snap *orderbook.Snapshot) error {
		reference := snap.BestAsk
		if ord.Side == orderbook.SideSell {
			reference = snap.BestBid
		}
		if reference == 0 {
			reference = ord.Price
		}
		notional := ord.Volume * reference
		if notional > limit {
			return fmt.Errorf("notional %.2f exceeds limit %.2f", notional, limit)
		}
		return nil
	}
}
