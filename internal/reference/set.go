package reference

import (
	"context"
	"sort"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// Set is a collection of reference books keyed by instrument. It routes a
// feed's book events to the right Book, creating books on first sight. Trade
// events are ignored here — the reference layer models the LOB only; the
// trade tape is a separate concern (Phase 5).
type Set struct {
	mu    sync.RWMutex
	books map[string]*Book
}

// NewSet returns an empty Set.
func NewSet() *Set {
	return &Set{books: make(map[string]*Book)}
}

// Get returns the book for instrument if it exists.
func (s *Set) Get(instrument string) (*Book, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.books[instrument]
	return b, ok
}

// Ensure returns the book for instrument, creating it (with exchange) if it
// does not yet exist. Useful for wiring a consumer (e.g. the seeder) to a book
// before any feed event has arrived.
func (s *Set) Ensure(instrument, exchange string) *Book {
	return s.book(instrument, exchange)
}

// book returns the book for instrument, creating it (with exchange) if absent.
func (s *Set) book(instrument, exchange string) *Book {
	s.mu.RLock()
	b, ok := s.books[instrument]
	s.mu.RUnlock()
	if ok {
		return b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok = s.books[instrument]; ok { // re-check under write lock
		return b
	}
	b = NewBook(instrument, exchange)
	s.books[instrument] = b
	return b
}

// Instruments returns the tracked instruments in sorted order.
func (s *Set) Instruments() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.books))
	for k := range s.books {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Apply routes a single event. Non-book events and book events without an
// instrument are ignored.
func (s *Set) Apply(ev feed.Event) {
	if ev.Kind != feed.EventBook || ev.Book == nil || ev.Book.Instrument == "" {
		return
	}
	s.book(ev.Book.Instrument, ev.Book.Exchange).Apply(ev.Book)
}

// Consume drains ch, applying every book event until ch is closed or ctx is
// cancelled. Intended to run in its own goroutine alongside a feed.Source.
func (s *Set) Consume(ctx context.Context, ch <-chan feed.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.Apply(ev)
		}
	}
}
