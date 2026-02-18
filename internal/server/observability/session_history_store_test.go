package observability

import (
	"path/filepath"
	"testing"
)

func TestSessionHistoryStore_PersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-history.jsonl")

	store1, err := NewSessionHistoryStore(path, 10, 100)
	if err != nil {
		t.Fatalf("new store1: %v", err)
	}
	store1.Push(SessionHistoryEntry{SessionID: "s1", Agent: "a1", Result: "ok"})
	store1.Push(SessionHistoryEntry{SessionID: "s2", Agent: "a1", Result: "ok"})
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := NewSessionHistoryStore(path, 10, 100)
	if err != nil {
		t.Fatalf("new store2: %v", err)
	}
	defer store2.Close()

	recent := store2.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(recent))
	}
	if recent[0].SessionID != "s1" || recent[1].SessionID != "s2" {
		t.Fatalf("unexpected order: %+v", recent)
	}
}
