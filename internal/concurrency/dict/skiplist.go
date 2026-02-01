package dict

import (
	"math/rand"
	"sync"
	"time"
)

const (
	maxLevel    = 16
	probability = 0.25
)

type skipNode struct {
	key     string
	value   any
	forward []*skipNode
}

// SkipListMap is a concurrent dictionary backed by a skip-list guarded with RWMutex.
type SkipListMap struct {
	head  *skipNode
	level int
	mu    sync.RWMutex
	rng   *rand.Rand
}

// NewSkipListMap returns an empty skip-list dictionary.
func NewSkipListMap() *SkipListMap {
	return &SkipListMap{
		head:  &skipNode{forward: make([]*skipNode, maxLevel)},
		level: 1,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *SkipListMap) Name() string { return "skiplist" }

func (s *SkipListMap) randomLevel() int {
	lvl := 1
	for lvl < maxLevel && s.rng.Float64() < probability {
		lvl++
	}
	return lvl
}

// Set inserts or replaces a key.
func (s *SkipListMap) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	update := make([]*skipNode, maxLevel)
	current := s.head
	for i := s.level - 1; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	next := current.forward[0]
	if next != nil && next.key == key {
		next.value = value
		return
	}

	lvl := s.randomLevel()
	if lvl > s.level {
		for i := s.level; i < lvl; i++ {
			update[i] = s.head
		}
		s.level = lvl
	}

	node := &skipNode{key: key, value: value, forward: make([]*skipNode, lvl)}
	for i := 0; i < lvl; i++ {
		node.forward[i] = update[i].forward[i]
		update[i].forward[i] = node
	}
}

// Get fetches a value.
func (s *SkipListMap) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	current := s.head
	for i := s.level - 1; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
	}

	next := current.forward[0]
	if next != nil && next.key == key {
		return next.value, true
	}
	return nil, false
}

// Delete removes a key.
func (s *SkipListMap) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	update := make([]*skipNode, maxLevel)
	current := s.head
	for i := s.level - 1; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	next := current.forward[0]
	if next == nil || next.key != key {
		return false
	}

	for i := 0; i < s.level; i++ {
		if update[i].forward[i] != next {
			break
		}
		update[i].forward[i] = next.forward[i]
	}

	for s.level > 1 && s.head.forward[s.level-1] == nil {
		s.level--
	}
	return true
}

// Len counts elements by traversing the bottom level.
func (s *SkipListMap) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	n := s.head.forward[0]
	for n != nil {
		count++
		n = n.forward[0]
	}
	return count
}

// Range iterates keys in ascending order.
func (s *SkipListMap) Range(fn func(key string, value any) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := s.head.forward[0]
	for n != nil {
		if !fn(n.key, n.value) {
			return
		}
		n = n.forward[0]
	}
}
