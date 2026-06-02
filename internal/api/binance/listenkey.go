package binance

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// listenKeyTTL is how long a user-data listenKey stays valid without a
// keepalive (PUT). Binance uses 60 minutes; we mirror that. The emulator keeps
// keys purely in memory, so a process restart invalidates all keys (acceptable
// for a test harness). Expiry is enforced lazily on lookup rather than swept by
// a timer, keeping the manager dependency-free.
const listenKeyTTL = 60 * time.Minute

// listenKeyManager tracks live user-data listenKeys for the single emulated
// account. It is safe for concurrent use.
type listenKeyManager struct {
	mu   sync.Mutex
	keys map[string]time.Time // key -> last-seen (created or keepalive) time
	now  func() time.Time
}

func newListenKeyManager(now func() time.Time) *listenKeyManager {
	if now == nil {
		now = time.Now
	}
	return &listenKeyManager{keys: make(map[string]time.Time), now: now}
}

// create mints a new random listenKey and records it. Binance returns the same
// key for repeated POSTs while one is active; for the emulator a fresh key per
// POST is acceptable and simpler.
func (m *listenKeyManager) create() string {
	var buf [32]byte
	_, _ = rand.Read(buf[:])
	key := hex.EncodeToString(buf[:])
	m.mu.Lock()
	m.keys[key] = m.now()
	m.mu.Unlock()
	return key
}

// valid reports whether key is live (exists and not past its TTL). Expired keys
// are pruned on lookup.
func (m *listenKeyManager) valid(key string) bool {
	if key == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen, ok := m.keys[key]
	if !ok {
		return false
	}
	if m.now().Sub(seen) > listenKeyTTL {
		delete(m.keys, key)
		return false
	}
	return true
}

// keepalive refreshes a key's last-seen time. Returns false if the key is not
// live (unknown or expired).
func (m *listenKeyManager) keepalive(key string) bool {
	if !m.valid(key) {
		return false
	}
	m.mu.Lock()
	m.keys[key] = m.now()
	m.mu.Unlock()
	return true
}

// remove deletes a key (DELETE /userDataStream). Returns false if absent.
func (m *listenKeyManager) remove(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[key]; !ok {
		return false
	}
	delete(m.keys, key)
	return true
}
