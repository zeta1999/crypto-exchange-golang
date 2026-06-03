// Package transfer moves balances between venue accounts (the API edges) by
// settling them ON CHAIN on testnet: a withdrawal debits the source venue's
// ledger and sends a real testnet payment from that venue's custody hot wallet
// to the destination venue's deposit address; a background watcher polls each
// venue's deposit address and credits its ledger when funds arrive. This lets an
// external arbitrage bot rebalance inventory across the two venues the way it
// would across real exchanges. Testnet-only; off unless configured.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/custody"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// sendPrecision is the on-chain decimal precision the hub quantizes to (Stellar
// uses 7). The ledger debit and the chain send use this same quantized amount.
const sendPrecision = 7

// ErrInsufficient is returned by Withdraw when the source ledger lacks the funds
// (so the API edge can map it to the right "insufficient balance" status).
var ErrInsufficient = errors.New("transfer: insufficient balance")

// Backend is the chain capability the hub needs: sign+broadcast payments and
// list incoming deposits. (internal/custody.Stellar satisfies it.)
type Backend interface {
	custody.Sender
	custody.Watcher
}

// venue is one trading venue with a ledger and an on-chain hot wallet.
type venue struct {
	id      string
	ledger  *account.Ledger
	secret  []byte // hot-wallet secret (decrypted seed) — used to sign sends
	address string // deposit address (also the hot wallet's address)
	cursor  string // last processed deposit paging token
}

// Transfer is an in-flight or settled cross-venue transfer (for visibility).
type Transfer struct {
	From, To, Asset, Amount, TxRef, Status string
}

// Hub coordinates withdrawals + deposits across venues over one chain backend.
type Hub struct {
	backend   Backend
	pollEvery time.Duration

	cursorPath string // optional: persist venue→cursor across restarts

	mu       sync.Mutex
	venues   map[string]*venue
	byAddr   map[string]*venue // deposit address -> venue (to credit on arrival)
	inflight []*Transfer
}

// NewHub returns a hub over the given chain backend, polling deposits every
// pollEvery (min 1s).
func NewHub(backend Backend, pollEvery time.Duration) *Hub {
	if pollEvery < time.Second {
		pollEvery = time.Second
	}
	return &Hub{
		backend:   backend,
		pollEvery: pollEvery,
		venues:    make(map[string]*venue),
		byAddr:    make(map[string]*venue),
	}
}

// AddVenue registers a venue's ledger + hot wallet. Call before Start.
func (h *Hub) AddVenue(id string, ledger *account.Ledger, secret []byte, address string) {
	v := &venue{id: id, ledger: ledger, secret: secret, address: address}
	h.venues[id] = v
	h.byAddr[address] = v
}

// Address returns a venue's deposit address (for native /transfer routing).
func (h *Hub) Address(venueID string) (string, bool) {
	v, ok := h.venues[venueID]
	if !ok {
		return "", false
	}
	return v.address, true
}

// Withdraw debits fromVenue's ledger and sends an on-chain payment of asset to
// destAddr. It returns the tx ref. The destination venue is credited later by
// the deposit watcher when the funds land (so balances reflect the in-flight
// period). Insufficient balance returns an error and sends nothing.
func (h *Hub) Withdraw(ctx context.Context, fromVenue, asset string, amount decimal.Decimal, destAddr string) (string, error) {
	h.mu.Lock()
	v := h.venues[fromVenue]
	h.mu.Unlock()
	if v == nil {
		return "", fmt.Errorf("transfer: unknown venue %q", fromVenue)
	}
	if amount.Sign() <= 0 {
		return "", fmt.Errorf("transfer: amount must be positive")
	}
	// Quantize to the chain's on-chain precision (Stellar: 7 dp) BEFORE debiting,
	// so the ledger debit and the on-chain send move exactly the same amount — a
	// full-precision debit + a 7-dp send would silently burn the difference.
	sendStr := amount.StringPrec(sendPrecision)
	sendAmt, err := decimal.Parse(sendStr)
	if err != nil {
		return "", fmt.Errorf("transfer: amount: %w", err)
	}
	if sendAmt.Sign() <= 0 {
		return "", fmt.Errorf("transfer: amount underflows chain precision (1e-%d)", sendPrecision)
	}
	if !v.ledger.Debit(asset, sendAmt) {
		return "", fmt.Errorf("%w: %s on %s", ErrInsufficient, asset, fromVenue)
	}
	txRef, err := h.backend.Send(ctx, v.secret, asset, destAddr, sendStr)
	if err != nil {
		v.ledger.Credit(asset, sendAmt) // send failed → refund the exact debit
		return "", fmt.Errorf("transfer: send: %w", err)
	}
	h.mu.Lock()
	to := ""
	if dv := h.byAddr[destAddr]; dv != nil { // map frozen after startup; lock for symmetry
		to = dv.id
	}
	h.inflight = append(h.inflight, &Transfer{From: fromVenue, To: to, Asset: asset, Amount: sendAmt.String(), TxRef: txRef, Status: "pending"})
	h.mu.Unlock()
	return txRef, nil
}

// Inflight returns a snapshot of recorded transfers.
func (h *Hub) Inflight() []Transfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Transfer, 0, len(h.inflight))
	for _, t := range h.inflight {
		out = append(out, *t)
	}
	return out
}

// SetCursorStore persists deposit cursors to path across restarts, so a deposit
// that lands while the process is down is still credited on the next run. When
// unset, cursors are in-memory only (a restart skips history). Call before Start.
func (h *Hub) SetCursorStore(path string) { h.cursorPath = path }

// Start seeds each venue's deposit cursor and runs the watch loop until ctx is
// done. A venue with a PERSISTED cursor resumes from it (crediting deposits that
// arrived while down); a venue with none skips its existing deposits (first run
// must not re-credit history).
func (h *Hub) Start(ctx context.Context) error {
	h.initCursors(ctx)
	h.saveCursors()
	t := time.NewTicker(h.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if h.poll(ctx) {
				h.saveCursors()
			}
		}
	}
}

// initCursors seeds cursors from the durable store; venues not found there are
// advanced past their existing deposits (so a first run doesn't credit history).
func (h *Hub) initCursors(ctx context.Context) {
	saved := h.loadCursors()
	for _, v := range h.venues {
		if c, ok := saved[v.id]; ok {
			v.cursor = c
			continue
		}
		if c, err := h.backend.LatestCursor(ctx, v.address); err == nil {
			v.cursor = c // skip existing history on a first run for this venue
		}
	}
}

// loadCursors reads the persisted venue→cursor map (empty if no store / missing).
func (h *Hub) loadCursors() map[string]string {
	out := map[string]string{}
	if h.cursorPath == "" {
		return out
	}
	data, err := os.ReadFile(h.cursorPath)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// saveCursors persists the current venue→cursor map (atomic temp+rename). Called
// only from the single watch goroutine, so no lock is needed on the cursors.
func (h *Hub) saveCursors() {
	if h.cursorPath == "" {
		return
	}
	m := make(map[string]string, len(h.venues))
	for id, v := range h.venues {
		m[id] = v.cursor
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := h.cursorPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("transfer: save cursors: %v", err)
		return
	}
	_ = os.Rename(tmp, h.cursorPath)
}

// poll checks each venue's deposit address and credits newly-arrived funds.
// It returns true if any cursor advanced (so the caller persists the cursors).
func (h *Hub) poll(ctx context.Context) bool {
	changed := false
	for _, v := range h.venues {
		pays, err := h.backend.Received(ctx, v.address, v.cursor)
		if err != nil {
			log.Printf("transfer: poll %s deposits: %v", v.id, err) // surface a stuck watcher
			continue
		}
		for _, p := range pays {
			amt, err := decimal.Parse(p.Amount)
			if err != nil {
				log.Printf("transfer: bad deposit amount %q on %s: %v", p.Amount, v.id, err)
				v.cursor = p.Cursor
				changed = true
				continue
			}
			v.ledger.Credit(p.Asset, amt)
			v.cursor = p.Cursor
			changed = true
			h.settle(p.TxRef)
			log.Printf("transfer: credited %s %s to %s (tx %s)", p.Amount, p.Asset, v.id, p.TxRef)
		}
	}
	return changed
}

func (h *Hub) settle(txRef string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.inflight {
		if t.TxRef == txRef {
			t.Status = "settled"
		}
	}
}
