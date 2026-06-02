package emulator

import (
	"sync"
	"testing"
	"time"
)

func TestControlsSeedAndRead(t *testing.T) {
	shift := NewPriceShift(10, 1)
	lat := NewLatency(LatencyConfig{FeedToBook: 5 * time.Millisecond}, 1)
	c := NewControls(shift, lat)

	if got := c.PriceShift(); got != shift {
		t.Fatalf("PriceShift() = %+v, want %+v", got, shift)
	}
	if c.Latency() != lat {
		t.Fatalf("Latency() did not return the seeded injector")
	}
}

func TestControlsSetSwaps(t *testing.T) {
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 1))

	c.SetPriceShift(NewPriceShift(50, 1))
	if got := c.PriceShift(); got.OffsetBps != 50 {
		t.Fatalf("after SetPriceShift, OffsetBps = %v, want 50", got.OffsetBps)
	}

	newLat := NewLatency(LatencyConfig{FeedToBook: 100 * time.Millisecond}, 2)
	c.SetLatency(newLat)
	if c.Latency() != newLat {
		t.Fatalf("SetLatency did not swap the injector")
	}
	if got := c.Latency().FeedToBookDelay(); got != 100*time.Millisecond {
		t.Fatalf("swapped latency delay = %v, want 100ms", got)
	}
}

// TestControlsConcurrentReadWrite exercises the lock-free read path against a
// concurrent writer; run under -race it must report no data race.
func TestControlsConcurrentReadWrite(t *testing.T) {
	c := NewControls(NewPriceShift(0, 1), NewLatency(LatencyConfig{}, 1))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: simulate the dispatcher + workers hammering the hot path.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.PriceShift()
					_ = c.Latency().FeedToBookDelay()
				}
			}
		}()
	}

	// Writer: the scenario runner mutating controls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			c.SetPriceShift(NewPriceShift(float64(i%100), 1))
			c.SetLatency(NewLatency(LatencyConfig{FeedToBook: time.Duration(i) * time.Microsecond}, int64(i)))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		close(stop)
	}()

	wg.Wait()
}
