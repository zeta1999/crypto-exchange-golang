package dict

import (
	"math/rand"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

type factory struct {
	name string
	fn   func() Dictionary
}

func implementations() []factory {
	return []factory{
		{name: "lockfree-tree", fn: func() Dictionary { return NewLockFreeTreeMap() }},
		{name: "lockfree-skiplist", fn: func() Dictionary { return NewLockFreeSkipListMap() }},
		{name: "skiplist", fn: func() Dictionary { return NewSkipListMap() }},
		{name: "sharded-map", fn: func() Dictionary { return NewShardedMap(32) }},
	}
}

func TestDictionaryBasicOperations(t *testing.T) {
	for _, impl := range implementations() {
		impl := impl
		t.Run(impl.name+"/SetGetDelete", func(t *testing.T) {
			d := impl.fn()
			d.Set("foo", 1)
			if val, ok := d.Get("foo"); !ok || val.(int) != 1 {
				t.Fatalf("expected 1 got %v %v", val, ok)
			}
			d.Set("bar", 2)
			if d.Len() != 2 {
				t.Fatalf("len mismatch: %d", d.Len())
			}
			if !d.Delete("foo") {
				t.Fatalf("expected delete to succeed")
			}
			if _, ok := d.Get("foo"); ok {
				t.Fatalf("key should be gone")
			}
		})
	}
}

func TestDictionaryRangeMatchesMap(t *testing.T) {
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) {
			d := impl.fn()
			expected := map[string]int{}
			for i := 0; i < 100; i++ {
				key := randomKey(i)
				d.Set(key, i)
				expected[key] = i
			}
			seen := map[string]int{}
			d.Range(func(k string, v any) bool {
				seen[k] = v.(int)
				return true
			})
			if len(seen) != len(expected) {
				t.Fatalf("range count mismatch expected=%d got=%d", len(expected), len(seen))
			}
			for k, v := range expected {
				if seen[k] != v {
					t.Fatalf("value mismatch for %s", k)
				}
			}
		})
	}
}

func TestDictionaryConcurrentAccess(t *testing.T) {
	for _, impl := range implementations() {
		t.Run(impl.name, func(t *testing.T) {
			d := impl.fn()
			workers := runtime.NumCPU()
			var wg sync.WaitGroup
			var errMu sync.Mutex
			errs := []string{}
			wg.Add(workers)
			for w := 0; w < workers; w++ {
				w := w
				go func() {
					defer wg.Done()
					rnd := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w)))
					for i := 0; i < 1000; i++ {
						key := randomKey(rnd.Intn(1_000))
						d.Set(key, i)
						if v, ok := d.Get(key); ok && v == nil {
							errMu.Lock()
							errs = append(errs, "nil value")
							errMu.Unlock()
						}
						if rnd.Intn(10) == 0 {
							d.Delete(key)
						}
					}
				}()
			}
			wg.Wait()
			if len(errs) > 0 {
				t.Fatalf("encountered concurrency errors: %v", errs)
			}

			keys := make([]string, 0, d.Len())
			d.Range(func(k string, _ any) bool {
				keys = append(keys, k)
				return true
			})
			if expectsSortedRange(d.Name()) && !sort.StringsAreSorted(keys) {
				t.Fatalf("Range should deliver sorted keys for %s", d.Name())
			}
		})
	}
}

func expectsSortedRange(name string) bool {
	switch name {
	case "lockfree-tree", "skiplist", "lockfree-skiplist":
		return true
	default:
		return false
	}
}

func BenchmarkDictionarySetGet(b *testing.B) {
	for _, impl := range implementations() {
		b.Run(impl.name, func(b *testing.B) {
			d := impl.fn()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := randomKey(i)
					d.Set(key, i)
					d.Get(key)
					i++
				}
			})
		})
	}
}

func randomKey(seed int) string {
	return string(rune('a'+(seed%26))) + string(rune('A'+(seed/26)%26)) + string(rune('0'+seed%10))
}
