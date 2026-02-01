# Concurrent Dictionary Experiments

This module collects several map variants that agents can benchmark while we decide what data structure the exchange should ship with. None of these types are production ready, but they capture trade‑offs worth documenting now.

## Lock-Free Copy-on-Write Tree

`LockFreeTreeMap` stores an immutable red-black style snapshot behind an `atomic.Pointer`. Reads simply load the current snapshot and are therefore wait-free. Writes clone the snapshot, apply the mutation, and publish it with `CompareAndSwap`. Because writers may contend on the CAS loop this structure is lock-free but **not wait-free** (a stalled goroutine cannot block readers, yet writers can starve under extreme contention).

## Skip-List Map

`SkipListMap` keeps keys in a probabilistic tower. We protect mutations with a single `RWMutex` to keep the implementation simple today. This means operations are blocking and neither lock-free nor wait-free, but lookups can run concurrently while inserts/deletes take the write lock.

### LockFreeSkipListMap

`LockFreeSkipListMap` mirrors the structure of the skip-list but replaces each forward pointer with an `atomic.Pointer`. Inserts clone per-level pointers and stitch nodes together with CAS loops, while deletes CAS each forward pointer away. Readers walk the level-0 linked list without taking locks, so lookups are wait-free. Writers remain lock-free (they retry when CAS fails) but can still starve under extreme contention.

## Sharded Map

`ShardedMap` is a pragmatic baseline that stripes a regular Go map across several shards guarded by independent locks. This is blocking, but the sharding keeps contention bounded. It is neither lock-free nor wait-free.

## Wait-Free Notes

A dictionary becomes wait-free when both readers and writers have bounded completion regardless of other goroutines. Achieving this typically requires per-node CAS operations and hazard-pointer style memory reclamation. The copy-on-write tree gets us halfway (wait-free reads) without implementing a full lockless red-black tree. If we need full wait-freeness in the future we should prototype a hazard-pointer skip-list or integrate an existing verified implementation.
