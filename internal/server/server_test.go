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

	w, err := NewAtomicWriter(dir, "test-agent", "test-backup", ".tar.gz")
	if err != nil {
		t.Fatalf("NewAtomicWriter: %v", err)
	}

	// Verifica que o diretório do agent/backup foi criado
	agentDir := filepath.Join(dir, "test-agent", "test-backup")
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		t.Fatal("backup directory not created")
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

	w, err := NewAtomicWriter(dir, "test-agent", "test-backup", ".tar.gz")
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
	removed, err := Rotate(dir, 3)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Verifica que os 4 mais antigos foram retornados como removidos
	if len(removed) != 4 {
		t.Errorf("expected 4 removed, got %d: %v", len(removed), removed)
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

	if _, err := Rotate(dir, 5); err != nil {
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

	if _, err := Rotate(dir, 1); err != nil {
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

	// PartialSession expirada (2h atrás, sem atividade recente)
	expiredPartial := &PartialSession{
		TmpPath:     tmpPath,
		AgentName:   "agent-a",
		StorageName: "storage-a",
		BaseDir:     dir,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	}
	expiredPartial.LastActivity.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	sessions.Store("partial-expired", expiredPartial)

	// PartialSession fresh (agora)
	freshPartial := &PartialSession{
		TmpPath:     filepath.Join(dir, "fresh.tmp"),
		AgentName:   "agent-b",
		StorageName: "storage-b",
		BaseDir:     dir,
		CreatedAt:   time.Now(),
	}
	freshPartial.LastActivity.Store(time.Now().UnixNano())
	sessions.Store("partial-fresh", freshPartial)

	// ParallelSession expirada (2h atrás, sem atividade recente)
	assembler, err := NewChunkAssembler("par-expired", dir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	expiredParallel := &ParallelSession{
		SessionID:   "par-expired",
		Assembler:   assembler,
		AgentName:   "agent-c",
		StorageName: "storage-c",
		MaxStreams:  4,
		Done:        make(chan struct{}),
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	}
	expiredParallel.LastActivity.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	sessions.Store("parallel-expired", expiredParallel)

	// ParallelSession fresh (agora)
	assembler2, err := NewChunkAssembler("par-fresh", dir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	freshParallel := &ParallelSession{
		SessionID:   "par-fresh",
		Assembler:   assembler2,
		AgentName:   "agent-d",
		StorageName: "storage-d",
		MaxStreams:  4,
		Done:        make(chan struct{}),
		CreatedAt:   time.Now(),
	}
	freshParallel.LastActivity.Store(time.Now().UnixNano())
	sessions.Store("parallel-fresh", freshParallel)

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

// TestCleanupExpiredSessions_ActiveSessionNotCleaned verifica que uma sessão
// com CreatedAt antigo mas LastActivity recente NÃO é limpa.
// Este é o cenário exato do bug: backup de ~42 GB levou >1h mas estava ativo.
func TestCleanupExpiredSessions_ActiveSessionNotCleaned(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	sessions := &sync.Map{}

	// PartialSession criada 2h atrás, mas com atividade 10s atrás
	activePartial := &PartialSession{
		TmpPath:     filepath.Join(dir, "active.tmp"),
		AgentName:   "agent-active",
		StorageName: "storage-active",
		BaseDir:     dir,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	}
	activePartial.LastActivity.Store(time.Now().Add(-10 * time.Second).UnixNano())
	os.WriteFile(activePartial.TmpPath, []byte("active data"), 0644)
	sessions.Store("partial-active", activePartial)

	// ParallelSession criada 3h atrás, mas com atividade 5s atrás
	assembler, err := NewChunkAssembler("par-active", dir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	activeParallel := &ParallelSession{
		SessionID:   "par-active",
		Assembler:   assembler,
		AgentName:   "agent-par-active",
		StorageName: "storage-par-active",
		MaxStreams:  12,
		Done:        make(chan struct{}),
		CreatedAt:   time.Now().Add(-3 * time.Hour),
	}
	activeParallel.LastActivity.Store(time.Now().Add(-5 * time.Second).UnixNano())
	sessions.Store("parallel-active", activeParallel)

	// Cleanup com TTL de 1h — NÃO deve limpar nenhuma sessão
	CleanupExpiredSessions(sessions, 1*time.Hour, logger)

	if _, ok := sessions.Load("partial-active"); !ok {
		t.Error("partial-active should NOT have been cleaned (LastActivity is recent)")
	}
	if _, ok := sessions.Load("parallel-active"); !ok {
		t.Error("parallel-active should NOT have been cleaned (LastActivity is recent)")
	}

	// Verifica que o tmp file da partial ativa NÃO foi removido
	if _, err := os.Stat(activePartial.TmpPath); os.IsNotExist(err) {
		t.Error("active tmp file should NOT have been removed")
	}
}
