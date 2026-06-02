package feed

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Source is a normalized market-data producer. Start spawns the ingestion
// goroutine(s) and returns a channel of Events; the channel is closed when
// ctx is cancelled (or the source is permanently exhausted, as with a
// finite replay file). Implementations own reconnection internally so the
// channel survives transient disconnects.
type Source interface {
	// Name is a stable identifier for the venue/source ("binance",
	// "coinbase", "replay").
	Name() string
	// Start begins streaming. It returns immediately with the read side of
	// the event channel; events are produced asynchronously.
	Start(ctx context.Context) (<-chan Event, error)
	// Status reports current health.
	Status() Status
}

// Status is a point-in-time health snapshot of a Source.
type Status struct {
	Name          string    `json:"name"`
	State         string    `json:"state"` // "disconnected" | "connected" | "reconnecting" | "stale" | "error" | "closed"
	LastUpdate    time.Time `json:"last_update"`
	LatencyMs     float64   `json:"latency_ms"`
	ErrorCount    uint64    `json:"error_count"`
	BytesReceived uint64    `json:"bytes_received"`
}

// Reconnect backoff bounds shared by the live adapters.
const (
	reconnectBase = 1 * time.Second
	reconnectMax  = 30 * time.Second
	// stableSession is how long a connection must last before its drop is
	// treated as a fresh failure (backoff reset) rather than a flapping one.
	stableSession = 1 * time.Minute
)

// RunReconnect drives connect in a loop until ctx is cancelled, applying
// capped exponential backoff between failures. connect is expected to block
// for the life of one connection and return nil only on ctx cancellation
// (a transient error triggers a reconnect). It centralizes the
// reconnect/backoff/ctx-check logic so each adapter's Start stays a thin
// goroutine that just closes its event channel on return.
func RunReconnect(ctx context.Context, t *StatusTracker, connect func(context.Context) error) {
	backoff := reconnectBase
	for {
		if ctx.Err() != nil {
			t.SetState("closed")
			return
		}
		start := time.Now()
		err := connect(ctx)
		if ctx.Err() != nil {
			t.SetState("closed")
			return
		}
		if err == nil {
			return
		}
		// A connection that survived a while was healthy; don't let an
		// eventual drop inherit an inflated backoff.
		if time.Since(start) > stableSession {
			backoff = reconnectBase
		}
		t.RecordError()
		t.SetState("reconnecting")
		slog.Warn("feed connection lost, reconnecting",
			"source", t.name, "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

// StatusTracker is a small concurrency-safe helper that the live adapters
// embed to avoid duplicating the same mutex-guarded counters. It is safe
// for concurrent use by the read (Status) and write (Record*) sides.
type StatusTracker struct {
	name string

	mu         sync.RWMutex
	state      string
	lastUpdate time.Time
	latencyMs  float64
	errorCount uint64
	bytesRecv  uint64
}

// NewStatusTracker returns a tracker in the "disconnected" state.
func NewStatusTracker(name string) *StatusTracker {
	return &StatusTracker{name: name, state: "disconnected"}
}

// SetState updates the lifecycle state ("connected", "reconnecting", ...).
func (t *StatusTracker) SetState(s string) {
	t.mu.Lock()
	t.state = s
	t.mu.Unlock()
}

// RecordUpdate notes a received message of the given size and the time it
// took to process, advancing LastUpdate to now.
func (t *StatusTracker) RecordUpdate(bytes int, latency time.Duration) {
	t.mu.Lock()
	t.lastUpdate = time.Now()
	t.latencyMs = float64(latency.Microseconds()) / 1000.0
	t.bytesRecv += uint64(bytes)
	t.mu.Unlock()
}

// RecordError increments the error counter.
func (t *StatusTracker) RecordError() {
	t.mu.Lock()
	t.errorCount++
	t.mu.Unlock()
}

// Status returns a snapshot of the tracked counters.
func (t *StatusTracker) Status() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Status{
		Name:          t.name,
		State:         t.state,
		LastUpdate:    t.lastUpdate,
		LatencyMs:     t.latencyMs,
		ErrorCount:    t.errorCount,
		BytesReceived: t.bytesRecv,
	}
}
