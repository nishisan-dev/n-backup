// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAtomicWriter_CommitAndAbort(t *testing.T) {
	dir := t.TempDir()

	w, err := NewAtomicWriter(dir, "test-agent")
	if err != nil {
		t.Fatalf("NewAtomicWriter: %v", err)
	}

	// Verifica que o diretório do agent foi criado
	agentDir := filepath.Join(dir, "test-agent")
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		t.Fatal("agent directory not created")
	}

	// Cria arquivo tmp
	f, tmpPath, err := w.TempFile()
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	f.Write([]byte("test data"))
	f.Close()

	// Verifica que .tmp existe
	if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
		t.Fatal("temp file not created")
	}

	// Commit
	finalPath, err := w.Commit(tmpPath)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verifica que o arquivo final existe
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		t.Fatal("final file not created after commit")
	}

	// Verifica que .tmp não existe mais
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("temp file should not exist after commit")
	}

	// Verifica extensão .tar.gz
	if filepath.Ext(finalPath) != ".gz" {
		t.Errorf("expected .gz extension, got %s", filepath.Ext(finalPath))
	}
}

func TestAtomicWriter_Abort(t *testing.T) {
	dir := t.TempDir()

	w, err := NewAtomicWriter(dir, "test-agent")
	if err != nil {
		t.Fatalf("NewAtomicWriter: %v", err)
	}

	f, tmpPath, err := w.TempFile()
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	f.Write([]byte("test data"))
	f.Close()

	// Abort
	if err := w.Abort(tmpPath); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	// .tmp deve ter sido removido
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("temp file should not exist after abort")
	}
}

func TestRotate_KeepsMaxBackups(t *testing.T) {
	dir := t.TempDir()

	// Cria 7 arquivos de backup
	names := []string{
		"2026-02-05T02-00-00.tar.gz",
		"2026-02-06T02-00-00.tar.gz",
		"2026-02-07T02-00-00.tar.gz",
		"2026-02-08T02-00-00.tar.gz",
		"2026-02-09T02-00-00.tar.gz",
		"2026-02-10T02-00-00.tar.gz",
		"2026-02-11T02-00-00.tar.gz",
	}

	for _, name := range names {
		os.WriteFile(filepath.Join(dir, name), []byte("data"), 0644)
	}

	// Rotação com max_backups = 3
	if err := Rotate(dir, 3); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Verifica que apenas 3 restam
	entries, _ := os.ReadDir(dir)
	var remaining []string
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}

	if len(remaining) != 3 {
		t.Errorf("expected 3 backups, got %d: %v", len(remaining), remaining)
	}

	// Os 3 mais recentes devem permanecer
	expected := []string{
		"2026-02-09T02-00-00.tar.gz",
		"2026-02-10T02-00-00.tar.gz",
		"2026-02-11T02-00-00.tar.gz",
	}

	for i, name := range expected {
		if remaining[i] != name {
			t.Errorf("expected %s at position %d, got %s", name, i, remaining[i])
		}
	}
}

func TestRotate_NoAction_UnderLimit(t *testing.T) {
	dir := t.TempDir()

	// Cria apenas 2 backups
	os.WriteFile(filepath.Join(dir, "2026-02-10T02-00-00.tar.gz"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "2026-02-11T02-00-00.tar.gz"), []byte("data"), 0644)

	if err := Rotate(dir, 5); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("expected 2 backups to remain, got %d", len(entries))
	}
}

func TestRotate_IgnoresNonTarGz(t *testing.T) {
	dir := t.TempDir()

	// Mistura de arquivos
	os.WriteFile(filepath.Join(dir, "2026-02-10T02-00-00.tar.gz"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "2026-02-11T02-00-00.tar.gz"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(dir, "backup-123.tmp"), []byte("data"), 0644)

	if err := Rotate(dir, 1); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	// Deve manter: 1 tar.gz + notes.txt + .tmp = 3
	if len(entries) != 3 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 3 files, got %d: %v", len(entries), names)
	}
}

func TestCleanupExpiredSessions_MixedTypes(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Cria um tmp file para a PartialSession expirada
	tmpPath := filepath.Join(dir, "expired.tmp")
	os.WriteFile(tmpPath, []byte("partial data"), 0644)

	sessions := &sync.Map{}

	// PartialSession expirada (2h atrás)
	sessions.Store("partial-expired", &PartialSession{
		TmpPath:     tmpPath,
		AgentName:   "agent-a",
		StorageName: "storage-a",
		BaseDir:     dir,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	})

	// PartialSession fresh (agora)
	sessions.Store("partial-fresh", &PartialSession{
		TmpPath:     filepath.Join(dir, "fresh.tmp"),
		AgentName:   "agent-b",
		StorageName: "storage-b",
		BaseDir:     dir,
		CreatedAt:   time.Now(),
	})

	// ParallelSession expirada (2h atrás) — esta causava o panic antes do fix
	assembler, err := NewChunkAssembler("par-expired", dir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	sessions.Store("parallel-expired", &ParallelSession{
		SessionID:   "par-expired",
		Assembler:   assembler,
		AgentName:   "agent-c",
		StorageName: "storage-c",
		MaxStreams:  4,
		Done:        make(chan struct{}),
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	})

	// ParallelSession fresh (agora)
	assembler2, err := NewChunkAssembler("par-fresh", dir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	sessions.Store("parallel-fresh", &ParallelSession{
		SessionID:   "par-fresh",
		Assembler:   assembler2,
		AgentName:   "agent-d",
		StorageName: "storage-d",
		MaxStreams:  4,
		Done:        make(chan struct{}),
		CreatedAt:   time.Now(),
	})

	// Deve NÃO dar panic
	CleanupExpiredSessions(sessions, 1*time.Hour, logger)

	// Verifica que sessões expiradas foram removidas
	if _, ok := sessions.Load("partial-expired"); ok {
		t.Error("partial-expired should have been cleaned up")
	}
	if _, ok := sessions.Load("parallel-expired"); ok {
		t.Error("parallel-expired should have been cleaned up")
	}

	// Verifica que sessões fresh foram mantidas
	if _, ok := sessions.Load("partial-fresh"); !ok {
		t.Error("partial-fresh should still exist")
	}
	if _, ok := sessions.Load("parallel-fresh"); !ok {
		t.Error("parallel-fresh should still exist")
	}

	// Verifica que o tmp file da partial expirada foi removido
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("expired tmp file should have been removed")
	}
}