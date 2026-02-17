// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEventStore_PushAndRecent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	store, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.PushEvent("info", "reconnect", "web-01", "stream reconnected", 0)
	store.PushEvent("warn", "rotate", "web-01", "flow rotated", 1)

	events := store.Recent(0)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "reconnect" {
		t.Errorf("expected first event 'reconnect', got %q", events[0].Type)
	}
	if events[1].Type != "rotate" {
		t.Errorf("expected second event 'rotate', got %q", events[1].Type)
	}

	// Verifica que o arquivo foi escrito
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty file")
	}
}

func TestEventStore_PersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Primeira instância: escreve eventos
	store1, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	store1.PushEvent("info", "test", "agent-1", "event-a", 0)
	store1.PushEvent("warn", "test", "agent-1", "event-b", 0)
	store1.PushEvent("error", "test", "agent-2", "event-c", 1)
	store1.Close()

	// Segunda instância: carrega eventos do arquivo
	store2, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	events := store2.Recent(0)
	if len(events) != 3 {
		t.Fatalf("expected 3 persisted events, got %d", len(events))
	}
	if events[0].Message != "event-a" {
		t.Errorf("expected 'event-a', got %q", events[0].Message)
	}
	if events[1].Message != "event-b" {
		t.Errorf("expected 'event-b', got %q", events[1].Message)
	}
	if events[2].Message != "event-c" {
		t.Errorf("expected 'event-c', got %q", events[2].Message)
	}

	// Verifica que novos eventos se somam
	store2.PushEvent("info", "test", "agent-1", "event-d", 0)
	events = store2.Recent(0)
	if len(events) != 4 {
		t.Fatalf("expected 4 events after append, got %d", len(events))
	}
}

func TestEventStore_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// maxLines = 10, então rotação mantém últimas 5
	store, err := NewEventStore(path, 100, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Push 15 eventos para forçar rotação
	for i := 0; i < 15; i++ {
		store.PushEvent("info", "test", "", "msg", 0)
	}
	store.Close()

	// Reabre e verifica que o arquivo foi rotacionado
	store2, err := NewEventStore(path, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	// Após rotação, arquivo deve ter ~5 linhas (maxLines/2)
	// Ring pode ter mais porque mantém tudo in-memory durante a vida do store1
	if store2.lineCount > 10 {
		t.Errorf("expected lineCount <= 10 after rotation, got %d", store2.lineCount)
	}
}

func TestEventStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Cria arquivo vazio
	os.WriteFile(path, []byte{}, 0644)

	store, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := store.Recent(0)
	if len(events) != 0 {
		t.Errorf("expected empty events, got %d", len(events))
	}
}

func TestEventStore_CorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Escreve um arquivo com linhas válidas e inválidas
	content := `{"timestamp":"2025-01-01T00:00:00Z","level":"info","type":"test","message":"ok"}
this is not json
{"timestamp":"2025-01-01T00:01:00Z","level":"warn","type":"test","message":"also ok"}
`
	os.WriteFile(path, []byte(content), 0644)

	store, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := store.Recent(0)
	if len(events) != 2 {
		t.Fatalf("expected 2 valid events (skipping corrupt line), got %d", len(events))
	}
	if events[0].Message != "ok" {
		t.Errorf("expected 'ok', got %q", events[0].Message)
	}
	if events[1].Message != "also ok" {
		t.Errorf("expected 'also ok', got %q", events[1].Message)
	}
}

func TestEventStore_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "events.jsonl")

	// Cria o subdiretório
	os.MkdirAll(filepath.Dir(path), 0755)

	store, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.PushEvent("info", "test", "", "hello", 0)
	events := store.Recent(0)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestEventStore_RingCapLimitOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Escreve 50 eventos
	store1, err := NewEventStore(path, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		store1.PushEvent("info", "test", "", "msg", 0)
	}
	store1.Close()

	// Reabre com ringCap=10 — deve carregar apenas os últimos 10
	store2, err := NewEventStore(path, 10, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	events := store2.Recent(0)
	if len(events) != 10 {
		t.Fatalf("expected 10 events in ring (capped), got %d", len(events))
	}
}
