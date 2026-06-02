// Package replay provides a deterministic, file-backed feed.Source and a
// Recorder that persists a live feed to disk.
//
// The on-disk format is JSON Lines: one feed.Event per line, in arrival
// order. Phase 1 replay is order-only (no wall-clock pacing); accelerated
// and real-time playback land in later phases. Reading a recorded file back
// reproduces the exact same Event sequence, which is what the deterministic
// tests rely on.
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// maxLineBytes bounds a single JSONL record. Depth-20 books are a few KB;
// 4 MB leaves generous headroom for full-depth captures without an
// unbounded buffer.
const maxLineBytes = 4 << 20

// Source replays events from a JSONL file (or any io.Reader via NewReader).
type Source struct {
	*feed.StatusTracker
	path string
	r    io.Reader
}

// New returns a Source that reads events from the file at path.
func New(path string) *Source {
	return &Source{StatusTracker: feed.NewStatusTracker("replay"), path: path}
}

// NewReader returns a Source that reads events from r. Useful for tests and
// in-memory fixtures; r is consumed once.
func NewReader(r io.Reader) *Source {
	return &Source{StatusTracker: feed.NewStatusTracker("replay"), r: r}
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
