// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestChunkAssembler_AssembleSingleStream(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-session-1", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Escreve dados no chunk file do stream 0
	f, _, err := ca.ChunkFile(0)
	if err != nil {
		t.Fatalf("ChunkFile(0): %v", err)
	}
	data := []byte("hello world from stream 0")
	if _, err := f.Write(data); err != nil {
		t.Fatalf("writing chunk: %v", err)
	}
	f.Close()

	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, ChunkSeq: 1, Offset: uint64(len(data)), Length: int64(len(data))})

	// Monta
	path, totalBytes, err := ca.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer os.Remove(path)

	if totalBytes != int64(len(data)) {
		t.Errorf("expected totalBytes=%d, got %d", len(data), totalBytes)
	}

	// Verifica conteúdo
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != string(data) {
		t.Errorf("expected %q, got %q", data, content)
	}
}

func TestChunkAssembler_AssembleMultiStream(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-session-2", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Stream 0
	f0, _, _ := ca.ChunkFile(0)
	d0 := []byte("AAAA")
	f0.Write(d0)
	f0.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, ChunkSeq: 1, Offset: uint64(len(d0)), Length: int64(len(d0))})

	// Stream 1
	f1, _, _ := ca.ChunkFile(1)
	d1 := []byte("BBBB")
	f1.Write(d1)
	f1.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 1, ChunkSeq: 1, Offset: uint64(len(d1)), Length: int64(len(d1))})

	// Stream 2
	f2, _, _ := ca.ChunkFile(2)
	d2 := []byte("CCCC")
	f2.Write(d2)
	f2.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 2, ChunkSeq: 1, Offset: uint64(len(d2)), Length: int64(len(d2))})

	// Monta
	path, totalBytes, err := ca.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer os.Remove(path)

	expectedTotal := int64(len(d0) + len(d1) + len(d2))
	if totalBytes != expectedTotal {
		t.Errorf("expected totalBytes=%d, got %d", expectedTotal, totalBytes)
	}

	// Conteúdo deve ser chunk_0 + chunk_1 + chunk_2 (ordem alfabética do nome)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	expected := "AAAABBBBCCCC"
	if string(content) != expected {
		t.Errorf("expected %q, got %q", expected, content)
	}
}

func TestChunkAssembler_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-session-3", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}

	chunkDir := ca.ChunkDir()

	// Verifica que o diretório foi criado
	if _, err := os.Stat(chunkDir); os.IsNotExist(err) {
		t.Fatal("chunk dir should exist before cleanup")
	}

	// Cleanup
	if err := ca.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Verifica que o diretório foi removido
	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Fatal("chunk dir should not exist after cleanup")
	}
}

func TestChunkAssembler_ChunkDir(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("my-session", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	expected := filepath.Join(tmpDir, "chunks_my-session")
	if ca.ChunkDir() != expected {
		t.Errorf("expected chunkDir=%q, got %q", expected, ca.ChunkDir())
	}
}
