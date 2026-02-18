package observability

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActiveSessionStore_FilterAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active-sessions.jsonl")

	store, err := NewActiveSessionStore(path, 100, 1000)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	now := time.Now()
	store.PushSnapshot(SessionSummary{SessionID: "s1", Agent: "a1", Mode: "parallel", Compression: "zst", BytesReceived: 10}, now)
	store.PushSnapshot(SessionSummary{SessionID: "s2", Agent: "a2", Mode: "single", Compression: "gzip", BytesReceived: 20}, now)
	store.PushSnapshot(SessionSummary{SessionID: "s1", Agent: "a1", Mode: "parallel", Compression: "zst", BytesReceived: 30}, now.Add(time.Minute))

	all := store.Recent(0, "")
	if len(all) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(all))
	}

	s1 := store.Recent(10, "s1")
	if len(s1) != 2 {
		t.Fatalf("expected 2 snapshots for s1, got %d", len(s1))
	}
	if s1[1].BytesReceived != 30 {
		t.Fatalf("expected latest bytes 30, got %d", s1[1].BytesReceived)
	}

	limited := store.Recent(1, "")
	if len(limited) != 1 || limited[0].SessionID != "s1" {
		t.Fatalf("expected only latest global snapshot, got %+v", limited)
	}
}
