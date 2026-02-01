package wal

import (
	"os"
	"testing"
)

func TestWALAppendAndReplay(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "wal-*.log")
	if err != nil {
		t.Fatalf("temp wal: %v", err)
	}
	path := tmp.Name()
	tmp.Close()

	wal, err := New(path)
	if err != nil {
		t.Fatalf("new wal: %v", err)
	}
	defer wal.Close()

	if err := wal.Append("order.limit", map[string]string{"id": "1"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := wal.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	count := 0
	err = Replay(path, func(e Entry) error {
		count++
		if e.Event != "order.limit" {
			t.Fatalf("unexpected event %s", e.Event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 entry got %d", count)
	}
}
