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

	// Escreve chunk com GlobalSeq 0
	f, path, err := ca.ChunkFileForSeq(0)
	if err != nil {
		t.Fatalf("ChunkFileForSeq(0): %v", err)
	}
	data := []byte("hello world from stream 0")
	if _, err := f.Write(data); err != nil {
		t.Fatalf("writing chunk: %v", err)
	}
	f.Close()

	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, GlobalSeq: 0, FilePath: path, Length: int64(len(data))})

	// Monta
	resultPath, totalBytes, err := ca.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != int64(len(data)) {
		t.Errorf("expected totalBytes=%d, got %d", len(data), totalBytes)
	}

	// Verifica conteúdo
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != string(data) {
		t.Errorf("expected %q, got %q", data, content)
	}
}

func TestChunkAssembler_AssembleMultiStream_GlobalOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-session-2", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Simula round-robin: chunk0→stream0, chunk1→stream1, chunk2→stream0
	// GlobalSeq 0: "AAAA" enviado pelo stream 0
	f0, p0, _ := ca.ChunkFileForSeq(0)
	f0.Write([]byte("AAAA"))
	f0.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, GlobalSeq: 0, FilePath: p0, Length: 4})

	// GlobalSeq 1: "BBBB" enviado pelo stream 1
	f1, p1, _ := ca.ChunkFileForSeq(1)
	f1.Write([]byte("BBBB"))
	f1.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 1, GlobalSeq: 1, FilePath: p1, Length: 4})

	// GlobalSeq 2: "CCCC" enviado pelo stream 0
	f2, p2, _ := ca.ChunkFileForSeq(2)
	f2.Write([]byte("CCCC"))
	f2.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, GlobalSeq: 2, FilePath: p2, Length: 4})

	// Monta — deve reconstruir na ordem GlobalSeq: AAAA BBBB CCCC
	resultPath, totalBytes, err := ca.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer os.Remove(resultPath)

	expectedTotal := int64(12)
	if totalBytes != expectedTotal {
		t.Errorf("expected totalBytes=%d, got %d", expectedTotal, totalBytes)
	}

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	expected := "AAAABBBBCCCC"
	if string(content) != expected {
		t.Errorf("expected %q, got %q", expected, content)
	}
}

func TestChunkAssembler_AssembleOutOfOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-session-ooo", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Registra chunks fora de ordem para verificar que Assemble reordena por GlobalSeq
	f2, p2, _ := ca.ChunkFileForSeq(2)
	f2.Write([]byte("CCCC"))
	f2.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 1, GlobalSeq: 2, FilePath: p2, Length: 4})

	f0, p0, _ := ca.ChunkFileForSeq(0)
	f0.Write([]byte("AAAA"))
	f0.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, GlobalSeq: 0, FilePath: p0, Length: 4})

	f1, p1, _ := ca.ChunkFileForSeq(1)
	f1.Write([]byte("BBBB"))
	f1.Close()
	ca.RegisterChunk(ChunkMeta{StreamIndex: 0, GlobalSeq: 1, FilePath: p1, Length: 4})

	resultPath, _, err := ca.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer os.Remove(resultPath)

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	// Deve estar na ordem GlobalSeq: AAAA BBBB CCCC
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
