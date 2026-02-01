package dict

import (
	"sort"
	"sync/atomic"
)

// LockFreeTreeMap offers wait-free reads and lock-free writes by cloning immutable snapshots.
type LockFreeTreeMap struct {
	ptr atomic.Pointer[treeSnapshot]
}

type treeSnapshot struct {
	keys  []string
	store map[string]any
}

// NewLockFreeTreeMap builds an empty map.
func NewLockFreeTreeMap() *LockFreeTreeMap {
	m := &LockFreeTreeMap{}
	snap := &treeSnapshot{store: make(map[string]any)}
	m.ptr.Store(snap)
	return m
}

// Name returns the implementation label.
func (m *LockFreeTreeMap) Name() string { return "lockfree-tree" }

func (m *LockFreeTreeMap) load() *treeSnapshot {
	if snap := m.ptr.Load(); snap != nil {
		return snap
	}
	snap := &treeSnapshot{store: make(map[string]any)}
	m.ptr.Store(snap)
	return snap
}

// Set stores or replaces a value.
func (m *LockFreeTreeMap) Set(key string, value any) {
	for {
		old := m.load()
		clone := old.clone()
		clone.set(key, value)
		if m.ptr.CompareAndSwap(old, clone) {
			return
		}
	}
}

// Get returns a value if present.
func (m *LockFreeTreeMap) Get(key string) (any, bool) {
	snap := m.load()
	val, ok := snap.store[key]
	return val, ok
}

// Delete removes a key.
func (m *LockFreeTreeMap) Delete(key string) bool {
	for {
		old := m.load()
		if _, ok := old.store[key]; !ok {
			return false
		}
		clone := old.clone()
		delete(clone.store, key)
		clone.rebuildKeys()
		if m.ptr.CompareAndSwap(old, clone) {
			return true
		}
	}
}

// Len returns the number of keys.
func (m *LockFreeTreeMap) Len() int {
	return len(m.load().store)
}

// Range iterates keys in sorted order.
func (m *LockFreeTreeMap) Range(fn func(key string, value any) bool) {
	snap := m.load()
	for _, k := range snap.keys {
		if !fn(k, snap.store[k]) {
			return
		}
	}
}

func (s *treeSnapshot) clone() *treeSnapshot {
	store := make(map[string]any, len(s.store))
	for k, v := range s.store {
		store[k] = v
	}
	keys := make([]string, len(s.keys))
	copy(keys, s.keys)
	return &treeSnapshot{store: store, keys: keys}
}

func (s *treeSnapshot) set(key string, value any) {
	if _, exists := s.store[key]; !exists {
		s.keys = append(s.keys, key)
		sort.Strings(s.keys)
	}
	s.store[key] = value
}

func (s *treeSnapshot) rebuildKeys() {
	s.keys = s.keys[:0]
	for k := range s.store {
		s.keys = append(s.keys, k)
	}
	sort.Strings(s.keys)
}
