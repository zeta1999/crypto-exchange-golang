// Package replay provides a deterministic, file-backed feed.Source and a
// Recorder that persists a live feed to disk.
//
// The on-disk format is JSON Lines: one feed.Event per line, in arrival
// order. By default replay is order-only (no wall-clock pacing), which is what
// the deterministic tests rely on; WithSpeed(>0) paces playback by the
// recorded inter-event timestamps (1.0 = real time, 10.0 = 10x). Pacing changes
// only timing, never event order or content, so reading a recorded file back
// always reproduces the exact same Event sequence.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// maxLineBytes bounds a single JSONL record. Depth-20 books are a few KB;
// 4 MB leaves generous headroom for full-depth captures without an
// unbounded buffer.
const maxLineBytes = 4 << 20

// Source replays events from a JSONL file (or any io.Reader via NewReader).
type Source struct {
	*feed.StatusTracker
	path  string
	r     io.Reader
	speed float64 // playback multiplier; <=0 = as-fast-as-possible (no pacing)
}

// Option configures a replay Source.
type Option func(*Source)

// WithSpeed sets the playback-speed multiplier. <=0 (the default) replays as
// fast as possible — order-only, no pacing, which the deterministic tests rely
// on. 1.0 reproduces the recorded inter-event wall-clock gaps; 10.0 plays 10x
// faster. Pacing affects only timing, never event order or content.
func WithSpeed(speed float64) Option {
	return func(s *Source) { s.speed = speed }
}

// New returns a Source that reads events from the file at path.
func New(path string, opts ...Option) *Source {
	s := &Source{StatusTracker: feed.NewStatusTracker("replay"), path: path}
	for _, o := range opts {
		o(s)
	}
	return s
}

// NewReader returns a Source that reads events from r. Useful for tests and
// in-memory fixtures; r is consumed once.
func NewReader(r io.Reader, opts ...Option) *Source {
	s := &Source{StatusTracker: feed.NewStatusTracker("replay"), r: r}
	for _, o := range opts {
		o(s)
	}
	return s
}

// eventTime returns the recorded timestamp carried by an event's payload, or
// the zero time if none is present (such an event is emitted without delay).
func eventTime(ev feed.Event) time.Time {
	switch {
	case ev.Book != nil:
		return ev.Book.Timestamp
	case ev.Trade != nil:
		return ev.Trade.Timestamp
	case ev.Ticker != nil:
		return ev.Ticker.Timestamp
	}
	return time.Time{}
}

func (s *Source) Name() string { return "replay" }

// Start streams the recorded events in order and closes the channel when
// the input is exhausted (or ctx is cancelled). A finite source: the
// channel close is the signal that replay is complete.
func (s *Source) Start(ctx context.Context) (<-chan feed.Event, error) {
	r := s.r
	var closer io.Closer
	if r == nil {
		f, err := os.Open(s.path)
		if err != nil {
			s.SetState("error")
			return nil, fmt.Errorf("replay open: %w", err)
		}
		r, closer = f, f
	}

	out := make(chan feed.Event, 1024)
	go func() {
		defer close(out)
		if closer != nil {
			defer closer.Close()
		}
		s.SetState("connected")

		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		// Pacing state: anchor the first recorded timestamp to the wall clock at
		// the moment we start emitting, then release each event when its recorded
		// offset (scaled by 1/speed) has elapsed. Only used when speed > 0.
		var firstTS, wallStart time.Time
		for sc.Scan() {
			if ctx.Err() != nil {
				s.SetState("closed")
				return
			}
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev feed.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				s.RecordError()
				continue
			}
			if s.speed > 0 {
				if ts := eventTime(ev); !ts.IsZero() {
					if firstTS.IsZero() {
						firstTS, wallStart = ts, time.Now()
					}
					// Target wall time for this event; a non-monotonic or earlier
					// timestamp yields a past target → no sleep.
					target := wallStart.Add(time.Duration(float64(ts.Sub(firstTS)) / s.speed))
					if d := time.Until(target); d > 0 {
						t := time.NewTimer(d)
						select {
						case <-t.C:
						case <-ctx.Done():
							t.Stop()
							s.SetState("closed")
							return
						}
					}
				}
			}
			s.RecordUpdate(len(line), 0)
			select {
			case out <- ev:
			case <-ctx.Done():
				s.SetState("closed")
				return
			}
		}
		if err := sc.Err(); err != nil {
			s.RecordError()
		}
		s.SetState("closed")
	}()
	return out, nil
}

// Recorder appends events to an io.Writer as JSON Lines. It is safe for
// concurrent use so multiple source goroutines can record to one file.
type Recorder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewRecorder writes JSONL records to w.
func NewRecorder(w io.Writer) *Recorder {
	return &Recorder{enc: json.NewEncoder(w)}
}

// Record persists a single event. json.Encoder.Encode appends a newline,
// yielding one record per line.
func (rec *Recorder) Record(ev feed.Event) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.enc.Encode(ev)
}
