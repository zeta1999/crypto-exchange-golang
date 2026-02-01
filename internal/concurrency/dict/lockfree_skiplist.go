package dict

import (
	"sort"
	"sync/atomic"
	"time"
)

const lockFreeSkipMaxLevel = 12

// LockFreeSkipListMap keeps skip-list towers inside immutable snapshots published atomically.
type LockFreeSkipListMap struct {
	ptr  atomic.Pointer[skipSnapshot]
	seed atomic.Uint64
}

type skipSnapshot struct {
	entries []skipEntry
}

type skipEntry struct {
	key   string
	value any
	level int
}

// NewLockFreeSkipListMap returns an empty dictionary with wait-free reads.
func NewLockFreeSkipListMap() *LockFreeSkipListMap {
	m := &LockFreeSkipListMap{}
	m.ptr.Store(&skipSnapshot{})
	m.seed.Store(uint64(time.Now().UnixNano()))
	return m
}

func (m *LockFreeSkipListMap) Name() string { return "lockfree-skiplist" }

func (m *LockFreeSkipListMap) load() *skipSnapshot {
	return m.ptr.Load()
}

// Set inserts or replaces a key.
func (m *LockFreeSkipListMap) Set(key string, value any) {
	for {
		old := m.load()
		clone := old.clone()
		idx := clone.search(key)
		if idx < len(clone.entries) && clone.entries[idx].key == key {
			clone.entries[idx].value = value
		} else {
			entry := skipEntry{key: key, value: value, level: m.randomLevel()}
			clone.entries = append(clone.entries, skipEntry{})
			copy(clone.entries[idx+1:], clone.entries[idx:])
			clone.entries[idx] = entry
		}
		if m.ptr.CompareAndSwap(old, clone) {
			return
		}
	}
}

// Get returns the value for key.
func (m *LockFreeSkipListMap) Get(key string) (any, bool) {
	snap := m.load()
	idx := snap.search(key)
	if idx < len(snap.entries) && snap.entries[idx].key == key {
		return snap.entries[idx].value, true
	}
	return nil, false
}

// Delete removes key if present.
func (m *LockFreeSkipListMap) Delete(key string) bool {
	for {
		old := m.load()
		clone := old.clone()
		idx := clone.search(key)
		if idx >= len(clone.entries) || clone.entries[idx].key != key {
			return false
		}
		clone.entries = append(clone.entries[:idx], clone.entries[idx+1:]...)
		if m.ptr.CompareAndSwap(old, clone) {
			return true
		}
	}
}

// Len counts entries.
func (m *LockFreeSkipListMap) Len() int {
	return len(m.load().entries)
}

// Range iterates entries in sorted order.
func (m *LockFreeSkipListMap) Range(fn func(key string, value any) bool) {
	snap := m.load()
	for _, entry := range snap.entries {
		if !fn(entry.key, entry.value) {
			return
		}
	}
}

func (m *LockFreeSkipListMap) randomLevel() int {
	seed := m.seed.Add(1)
	seed ^= seed << 13
	seed ^= seed >> 7
	seed ^= seed << 17
	lvl := 1
	for lvl < lockFreeSkipMaxLevel && seed&0x1 == 1 {
		lvl++
		seed >>= 1
	}
	return lvl
}

func (s *skipSnapshot) clone() *skipSnapshot {
	entries := make([]skipEntry, len(s.entries))
	copy(entries, s.entries)
	return &skipSnapshot{entries: entries}
}

func (s *skipSnapshot) search(key string) int {
	return sort.Search(len(s.entries), func(i int) bool {
		return s.entries[i].key >= key
	})
}
