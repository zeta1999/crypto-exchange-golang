package dict

import (
	"hash/fnv"
	"sync"
)

// ShardedMap stripes keys across multiple shards guarded by RWMutexes.
type ShardedMap struct {
	shards []shard
}

type shard struct {
	mu   sync.RWMutex
	data map[string]any
}

// NewShardedMap creates a dictionary with n shards (default 32 when n<=0).
func NewShardedMap(n int) *ShardedMap {
	if n <= 0 {
		n = 32
	}
	s := &ShardedMap{shards: make([]shard, n)}
	for i := range s.shards {
		s.shards[i].data = make(map[string]any)
	}
	return s
}

func (s *ShardedMap) Name() string { return "sharded-map" }

func (s *ShardedMap) bucket(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := h.Sum32() % uint32(len(s.shards))
	return &s.shards[idx]
}

func (s *ShardedMap) Set(key string, value any) {
	sh := s.bucket(key)
	sh.mu.Lock()
	sh.data[key] = value
	sh.mu.Unlock()
}

func (s *ShardedMap) Get(key string) (any, bool) {
	sh := s.bucket(key)
	sh.mu.RLock()
	val, ok := sh.data[key]
	sh.mu.RUnlock()
	return val, ok
}

func (s *ShardedMap) Delete(key string) bool {
	sh := s.bucket(key)
	sh.mu.Lock()
	_, ok := sh.data[key]
	if ok {
		delete(sh.data, key)
	}
	sh.mu.Unlock()
	return ok
}

func (s *ShardedMap) Len() int {
	total := 0
	for i := range s.shards {
		s.shards[i].mu.RLock()
		total += len(s.shards[i].data)
		s.shards[i].mu.RUnlock()
	}
	return total
}

func (s *ShardedMap) Range(fn func(key string, value any) bool) {
	for i := range s.shards {
		s.shards[i].mu.RLock()
		for k, v := range s.shards[i].data {
			if !fn(k, v) {
				s.shards[i].mu.RUnlock()
				return
			}
		}
		s.shards[i].mu.RUnlock()
	}
}
