package wal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is a single WAL record persisted on disk.
type Entry struct {
	Timestamp time.Time       `json:"ts"`
	Event     string          `json:"event"`
	Payload   json.RawMessage `json:"payload"`
}

// Appender defines the minimal WAL functionality used by the engine.
type Appender interface {
	Append(event string, payload interface{}) error
}

// WAL writes append-only JSON entries for durable recovery.
type WAL struct {
	mu         sync.Mutex
	file       *os.File
	writer     *bufio.Writer
	flushEvery int
	pending    int
}

// New creates (or opens) the WAL file at the given path.
func New(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create wal dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	return &WAL{
		file:       file,
		writer:     bufio.NewWriterSize(file, 64*1024),
		flushEvery: 32,
	}, nil
}

// Append persists an event payload and flushes periodically for efficiency.
func (w *WAL) Append(event string, payload interface{}) error {
	if w == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	entry := Entry{Timestamp: time.Now().UTC(), Event: event, Payload: raw}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.writer.Write(encoded); err != nil {
		return err
	}
	if err := w.writer.WriteByte('\n'); err != nil {
		return err
	}
	w.pending++
	if w.pending >= w.flushEvery {
		if err := w.writer.Flush(); err != nil {
			return err
		}
		w.pending = 0
	}
	return nil
}

// Flush ensures all buffered writes reach disk.
func (w *WAL) Flush() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = 0
	return w.writer.Flush()
}

// Close flushes outstanding data and closes the file handle.
func (w *WAL) Close() error {
	if w == nil {
		return nil
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return w.file.Close()
}

// Replay streams entries from disk into fn for recovery/testing.
func Replay(path string, fn func(Entry) error) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open wal for replay: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("decode entry: %w", err)
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Nop returns an appender that drops entries, handy for tests.
func Nop() Appender {
	return nopAppender{}
}

type nopAppender struct{}

func (nopAppender) Append(string, interface{}) error { return nil }
