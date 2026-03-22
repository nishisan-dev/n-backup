// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBucketUploadStore_PushAndRecent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bucket-uploads.jsonl")

	store, err := NewBucketUploadStore(path, 100, 10000)
	if err != nil {
		t.Fatalf("NewBucketUploadStore: %v", err)
	}
	defer store.Close()

	store.Push(BucketUploadEntry{
		Agent:      "web-01",
		Storage:    "main",
		Backup:     "home",
		BucketName: "contaboo",
		Mode:       "sync",
		Success:    true,
		Duration:   "5m30s",
	})
	store.Push(BucketUploadEntry{
		Agent:      "web-01",
		Storage:    "main",
		Backup:     "home",
		BucketName: "contaboo",
		Mode:       "sync",
		Success:    false,
		Error:      "credentials not set",
		Duration:   "0s",
	})

	recent := store.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(recent))
	}
	if recent[0].Success != true {
		t.Error("first entry should be success=true")
	}
	if recent[1].Success != false {
		t.Error("second entry should be success=false")
	}
	if recent[1].Error != "credentials not set" {
		t.Errorf("second entry error: got %q, want %q", recent[1].Error, "credentials not set")
	}
}

func TestBucketUploadStore_PersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bucket-uploads.jsonl")

	store1, err := NewBucketUploadStore(path, 100, 10000)
	if err != nil {
		t.Fatalf("NewBucketUploadStore: %v", err)
	}
	store1.Push(BucketUploadEntry{Agent: "a1", BucketName: "b1", Mode: "sync", Success: true, Duration: "1s"})
	store1.Push(BucketUploadEntry{Agent: "a2", BucketName: "b2", Mode: "offload", Success: false, Error: "fail", Duration: "2s"})
	store1.Close()

	store2, err := NewBucketUploadStore(path, 100, 10000)
	if err != nil {
		t.Fatalf("NewBucketUploadStore (reload): %v", err)
	}
	defer store2.Close()

	recent := store2.Recent(0)
	if len(recent) != 2 {
		t.Fatalf("expected 2 persisted entries, got %d", len(recent))
	}
	if recent[0].Agent != "a1" || recent[1].Agent != "a2" {
		t.Errorf("unexpected agents: %q, %q", recent[0].Agent, recent[1].Agent)
	}
}

func TestBucketUploadStore_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bucket-uploads.jsonl")

	store, err := NewBucketUploadStore(path, 100, 10)
	if err != nil {
		t.Fatalf("NewBucketUploadStore: %v", err)
	}

	for i := 0; i < 15; i++ {
		store.Push(BucketUploadEntry{Agent: "a1", BucketName: "b1", Mode: "sync", Success: true, Duration: "1s"})
	}
	store.Close()

	// Reabre e verifica que arquivo foi rotacionado (≤ maxLines/2 = 5)
	store2, err := NewBucketUploadStore(path, 100, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	recent := store2.Recent(0)
	if len(recent) > 10 {
		t.Errorf("expected ≤10 entries after rotation, got %d", len(recent))
	}
}

func TestBucketUploadStore_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "bucket-uploads.jsonl")

	// Cria subdir necessário
	os.MkdirAll(filepath.Dir(path), 0755)

	store, err := NewBucketUploadStore(path, 100, 10000)
	if err != nil {
		t.Fatalf("NewBucketUploadStore: %v", err)
	}
	defer store.Close()

	store.Push(BucketUploadEntry{Agent: "a1", BucketName: "b1", Mode: "sync", Success: true, Duration: "1s"})
	if store.Recent(0)[0].Agent != "a1" {
		t.Error("push/recent failed with new file")
	}
}

func TestBucketUploadRing_Overflow(t *testing.T) {
	ring := NewBucketUploadRing(5)

	for i := 0; i < 10; i++ {
		ring.Push(BucketUploadEntry{Agent: "a1", BucketName: "b1", Mode: "sync", Success: true, Duration: "1s"})
	}

	if ring.Len() != 5 {
		t.Errorf("expected len=5, got %d", ring.Len())
	}

	recent := ring.Recent(0)
	if len(recent) != 5 {
		t.Errorf("expected 5 entries, got %d", len(recent))
	}
}
