// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestChunkAssembler_WriteChunk_InOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-inorder", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Escreve 3 chunks in-order
	chunks := []string{"AAAA", "BBBB", "CCCC"}
	for i, data := range chunks {
		r := bytes.NewReader([]byte(data))
		if err := ca.WriteChunk(uint32(i), r, int64(len(data))); err != nil {
			t.Fatalf("WriteChunk(%d): %v", i, err)
		}
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
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

func TestChunkAssembler_WriteChunk_OutOfOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-ooo", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Chunks chegam fora de ordem: 2, 0, 1
	if err := ca.WriteChunk(2, bytes.NewReader([]byte("CCCC")), 4); err != nil {
		t.Fatalf("WriteChunk(2): %v", err)
	}
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AAAA")), 4); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	// Ao escrever chunk 1, os chunks 1 e 2 devem ser flushed
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BBBB")), 4); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 12 {
		t.Errorf("expected totalBytes=12, got %d", totalBytes)
	}

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

func TestChunkAssembler_WriteChunk_MultiStream_RoundRobin(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-rr", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Simula round-robin: stream 0 recebe seq 0,2,4; stream 1 recebe seq 1,3,5
	// Mas chegam intercalados: 0, 1, 2, 3, 4, 5 → todos in-order
	for i := 0; i < 6; i++ {
		data := []byte{byte('A' + i), byte('A' + i)}
		if err := ca.WriteChunk(uint32(i), bytes.NewReader(data), 2); err != nil {
			t.Fatalf("WriteChunk(%d): %v", i, err)
		}
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 12 {
		t.Errorf("expected totalBytes=12, got %d", totalBytes)
	}

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	expected := "AABBCCDDEEFF"
	if string(content) != expected {
		t.Errorf("expected %q, got %q", expected, content)
	}
}

func TestChunkAssembler_OutOfOrder_UsesShardedChunkPath(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithMemLimit("test-shard", tmpDir, logger, 1)
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithMemLimit: %v", err)
	}
	defer ca.Cleanup()

	const seq uint32 = 513 // 0x0201 -> level1=01, level2=02
	if err := ca.WriteChunk(seq, bytes.NewReader([]byte("ZZ")), 2); err != nil {
		t.Fatalf("WriteChunk(%d): %v", seq, err)
	}

	expectedPath := filepath.Join(ca.ChunkDir(), "01", "02", "chunk_0000000513.tmp")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected sharded chunk file at %q: %v", expectedPath, err)
	}
}

func TestChunkAssembler_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Limite baixo para forçar spill em disco no out-of-order deste teste.
	ca, err := NewChunkAssemblerWithMemLimit("test-cleanup", tmpDir, logger, 1)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}

	// Escreve um chunk out-of-order para criar o chunkDir
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("XX")), 2); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}

	chunkDir := ca.ChunkDir()

	// Verifica que o diretório de chunks foi criado (lazy creation)
	if _, err := os.Stat(chunkDir); os.IsNotExist(err) {
		t.Fatal("chunk dir should exist after out-of-order write")
	}

	// Cleanup
	if err := ca.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Verifica que o diretório de chunks foi removido
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

func TestChunkAssembler_LazyMode_FinalAssembly(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-lazy-mode", tmpDir, logger, ChunkAssemblerOptions{
		Mode: AssemblerModeLazy,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	// Ordem de chegada intencionalmente fora de ordem.
	if err := ca.WriteChunk(2, bytes.NewReader([]byte("CCCC")), 4); err != nil {
		t.Fatalf("WriteChunk(2): %v", err)
	}
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AAAA")), 4); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BBBB")), 4); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 12 {
		t.Errorf("expected totalBytes=12, got %d", totalBytes)
	}

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AAAABBBBCCCC" {
		t.Errorf("expected %q, got %q", "AAAABBBBCCCC", content)
	}
}

func TestChunkAssembler_LazyChunkDir_NotCreatedForInOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssembler("test-lazy", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer ca.Cleanup()

	// Escreve chunks in-order — o chunkDir NÃO deve ser criado
	for i := 0; i < 5; i++ {
		if err := ca.WriteChunk(uint32(i), bytes.NewReader([]byte("XX")), 2); err != nil {
			t.Fatalf("WriteChunk(%d): %v", i, err)
		}
	}

	chunkDir := ca.ChunkDir()
	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Fatal("chunk dir should NOT exist for in-order-only writes")
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 10 {
		t.Errorf("expected totalBytes=10, got %d", totalBytes)
	}
}

func TestChunkAssembler_EagerDiskSpill_Sharded_StillAssembles(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithMemLimit("test-eager-sharded-assemble", tmpDir, logger, 1)
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithMemLimit: %v", err)
	}
	defer ca.Cleanup()

	// seq=2 força spill em disco (limite 1 byte), indo para shard "02".
	if err := ca.WriteChunk(2, bytes.NewReader([]byte("CC")), 2); err != nil {
		t.Fatalf("WriteChunk(2): %v", err)
	}
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BB")), 2); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}

	expectedPath := filepath.Join(ca.ChunkDir(), "02", "00", "chunk_0000000002.tmp")
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Fatalf("chunk file should have been consumed and removed after flush: %v", err)
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 6 {
		t.Fatalf("expected totalBytes=6, got %d", totalBytes)
	}

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AABBCC" {
		t.Fatalf("expected %q, got %q", "AABBCC", content)
	}
}

func TestChunkAssembler_LazyMode_ShardedPaths_StillAssembles(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-lazy-sharded-assemble", tmpDir, logger, ChunkAssemblerOptions{Mode: AssemblerModeLazy})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BBBB")), 4); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AAAA")), 4); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 8 {
		t.Fatalf("expected totalBytes=8, got %d", totalBytes)
	}

	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AAAABBBB" {
		t.Fatalf("expected %q, got %q", "AAAABBBB", content)
	}
}

func TestChunkAssembler_TwoLevelSharding_LargeSeq(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithMemLimit("test-two-level-large", tmpDir, logger, 1)
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithMemLimit: %v", err)
	}
	defer ca.Cleanup()

	// seq=66051 = 0x010203 -> level1: 66051%256=3 -> "03", level2: (66051/256)%256=258%256=2 -> "02"
	const seq uint32 = 66051
	if err := ca.WriteChunk(seq, bytes.NewReader([]byte("XX")), 2); err != nil {
		t.Fatalf("WriteChunk(%d): %v", seq, err)
	}

	expectedPath := filepath.Join(ca.ChunkDir(), "03", "02", "chunk_0000066051.tmp")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected two-level sharded chunk file at %q: %v", expectedPath, err)
	}
}
