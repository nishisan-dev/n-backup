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
	"runtime"
	"sync"
	"sync/atomic"
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

	// ShardLevels default (1) — apenas 1 nível de diretório
	ca, err := NewChunkAssemblerWithMemLimit("test-shard", tmpDir, logger, 1)
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithMemLimit: %v", err)
	}
	defer ca.Cleanup()

	const seq uint32 = 513 // 513%256=1 -> "01"
	if err := ca.WriteChunk(seq, bytes.NewReader([]byte("ZZ")), 2); err != nil {
		t.Fatalf("WriteChunk(%d): %v", seq, err)
	}

	// Com 1 nível: chunks_session/01/chunk_xxx.tmp
	expectedPath := filepath.Join(ca.ChunkDir(), "01", "chunk_0000000513.tmp")
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

	// ShardLevels default (1) — 1 nível
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

	// Com 1 nível: chunks_session/02/chunk_xxx.tmp (já consumido)
	expectedPath := filepath.Join(ca.ChunkDir(), "02", "chunk_0000000002.tmp")
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

	// Explicitamente 2 níveis
	ca, err := NewChunkAssemblerWithOptions("test-two-level-large", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: 1,
		ShardLevels:     2,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
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

func TestChunkAssembler_SingleLevelSharding(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-single-level", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: 1,
		ShardLevels:     1,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	// seq=66051 -> 66051%256=3 -> "03" (apenas 1 nível)
	const seq uint32 = 66051
	if err := ca.WriteChunk(seq, bytes.NewReader([]byte("XX")), 2); err != nil {
		t.Fatalf("WriteChunk(%d): %v", seq, err)
	}

	expectedPath := filepath.Join(ca.ChunkDir(), "03", "chunk_0000066051.tmp")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected single-level sharded chunk file at %q: %v", expectedPath, err)
	}
}

func TestChunkAssembler_MkdirAllCache(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-cache", tmpDir, logger, ChunkAssemblerOptions{
		Mode:        AssemblerModeLazy,
		ShardLevels: 1,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	// Dois chunks no mesmo shard (seq%256 == 0 para ambos)
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if err := ca.WriteChunk(256, bytes.NewReader([]byte("BB")), 2); err != nil {
		t.Fatalf("WriteChunk(256): %v", err)
	}

	// Ambos devem estar no shard "00"
	p0 := filepath.Join(ca.ChunkDir(), "00", "chunk_0000000000.tmp")
	p256 := filepath.Join(ca.ChunkDir(), "00", "chunk_0000000256.tmp")
	if _, err := os.Stat(p0); err != nil {
		t.Fatalf("expected chunk 0 at %q: %v", p0, err)
	}
	if _, err := os.Stat(p256); err != nil {
		t.Fatalf("expected chunk 256 at %q: %v", p256, err)
	}

	// Cache deve ter exatamente 1 entrada (mesmo shard)
	ca.mu.Lock()
	cacheSize := len(ca.createdShards)
	ca.mu.Unlock()
	if cacheSize != 1 {
		t.Errorf("expected createdShards cache to have 1 entry, got %d", cacheSize)
	}
}

// --- Fix C: revalidação pós-lock no saveOutOfOrder ---

// TestChunkAssembler_SpillRaceWindow_ConcurrentInOrder verifica o race window
// do saveOutOfOrder: chunk out-of-order começa spill, outro goroutine preenche
// o gap. Deve resultar em assembly correto (race detector ativo).
func TestChunkAssembler_SpillRaceWindow_ConcurrentInOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// pendingMemLimit=1 força spill em disco para qualquer chunk out-of-order.
	ca, err := NewChunkAssemblerWithOptions("test-spill-race", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: 1,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	// Goroutine A: WriteChunk(1) — out-of-order, fará spill para disco.
	// Goroutine B (main): WriteChunk(0) — preenche o gap enquanto A está no I/O externo.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ca.WriteChunk(1, bytes.NewReader([]byte("BB")), 2); err != nil {
			t.Errorf("WriteChunk(1): %v", err)
		}
	}()

	// Cede o scheduler para que a goroutine A inicie o spill antes de B escrever.
	runtime.Gosched()

	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	wg.Wait()

	// Chunk 2: in-order após flusharmos o pendente.
	if err := ca.WriteChunk(2, bytes.NewReader([]byte("CC")), 2); err != nil {
		t.Fatalf("WriteChunk(2): %v", err)
	}

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 6 {
		t.Errorf("expected totalBytes=6, got %d", totalBytes)
	}
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AABBCC" {
		t.Errorf("expected %q, got %q", "AABBCC", content)
	}
}

// TestChunkAssembler_SpillPromotedToInOrder verifica o caso (c) do passo 3:
// globalSeq == nextExpected após re-lock → chunk promovido direto ao outBuf.
// Verifica também que nenhum arquivo spill-*.tmp fica esquecido no shardDir.
func TestChunkAssembler_SpillPromotedToInOrder(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// pendingMemLimit=1 força spill. shardLevels=1 para path previsível.
	ca, err := NewChunkAssemblerWithOptions("test-promoted", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: 1,
		ShardLevels:     1,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ca.WriteChunk(1, bytes.NewReader([]byte("BB")), 2); err != nil {
			t.Errorf("WriteChunk(1): %v", err)
		}
	}()

	runtime.Gosched()
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	wg.Wait()

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 4 {
		t.Errorf("expected totalBytes=4, got %d", totalBytes)
	}
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AABB" {
		t.Errorf("expected %q, got %q", "AABB", content)
	}

	// Após finalizar, nenhum arquivo spill-*.tmp deve existir no shard dir.
	shardDir := filepath.Join(ca.ChunkDir(), "01")
	if _, err := os.Stat(shardDir); os.IsNotExist(err) {
		return // shard dir nunca criado (promoção antes do rename) — OK
	}
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatalf("reading shard dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("unexpected leftover file in shard dir: %s", e.Name())
		}
	}
}

// TestChunkAssembler_SpillConcurrent_Race valida correctness e ausência de race
// com múltiplas goroutines escrevendo chunks simultâneos (mix in-order e out-of-order).
// Deve ser executado com: go test -race
func TestChunkAssembler_SpillConcurrent_Race(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// pendingMemLimit=1 garante spill em disco para todos os out-of-order.
	ca, err := NewChunkAssemblerWithOptions("test-spill-concurrent", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: 1,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	const n = 8
	data := make([][]byte, n)
	totalExpected := 0
	for i := 0; i < n; i++ {
		data[i] = []byte{byte('A' + i), byte('A' + i)}
		totalExpected += len(data[i])
	}

	// Escreve todos os chunks em paralelo — ordem de chegada não determinística.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			if err := ca.WriteChunk(uint32(seq), bytes.NewReader(data[seq]), int64(len(data[seq]))); err != nil {
				t.Errorf("WriteChunk(%d): %v", seq, err)
			}
		}(i)
	}
	wg.Wait()

	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	defer os.Remove(resultPath)

	if int(totalBytes) != totalExpected {
		t.Errorf("expected totalBytes=%d, got %d", totalExpected, totalBytes)
	}
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	// Conteúdo deve estar ordenado por globalSeq: AA BB CC DD EE FF GG HH
	expected := "AABBCCDDEEFFGGHH"
	if string(content) != expected {
		t.Errorf("expected %q, got %q", expected, content)
	}
}

func TestChunkAssembler_ChunkFsync_LazyEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-fsync-lazy-enabled", tmpDir, logger, ChunkAssemblerOptions{
		Mode:             AssemblerModeLazy,
		FsyncChunkWrites: true,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	var syncCalls atomic.Int32
	orig := syncFile
	syncFile = func(f *os.File) error {
		syncCalls.Add(1)
		return nil
	}
	defer func() { syncFile = orig }()

	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if got := syncCalls.Load(); got < 1 {
		t.Fatalf("expected syncFile to be called at least once, got %d", got)
	}
}

func TestChunkAssembler_ChunkFsync_EagerSpillEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-fsync-spill-enabled", tmpDir, logger, ChunkAssemblerOptions{
		Mode:             AssemblerModeEager,
		PendingMemLimit:  1, // força spill para disco em out-of-order
		FsyncChunkWrites: true,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	var syncCalls atomic.Int32
	orig := syncFile
	syncFile = func(f *os.File) error {
		syncCalls.Add(1)
		return nil
	}
	defer func() { syncFile = orig }()

	// out-of-order sem preencher gap: persiste em staging via spill.
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BB")), 2); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}
	if got := syncCalls.Load(); got < 1 {
		t.Fatalf("expected syncFile to be called at least once on spill write, got %d", got)
	}
}

func TestChunkAssembler_ChunkFsync_Disabled(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-fsync-disabled", tmpDir, logger, ChunkAssemblerOptions{
		Mode:             AssemblerModeLazy,
		FsyncChunkWrites: false,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	var syncCalls atomic.Int32
	orig := syncFile
	syncFile = func(f *os.File) error {
		syncCalls.Add(1)
		return nil
	}
	defer func() { syncFile = orig }()

	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if got := syncCalls.Load(); got != 0 {
		t.Fatalf("expected syncFile to not be called when fsync disabled, got %d", got)
	}
}

// TestChunkAssembler_LazyMode_WriteFailure_LazyMaxSeqNotCorrupted é o teste de
// regressão para o bug encontrado em produção após 16h de backup paralelo:
//
//	"missing chunk seq 434628 in lazy assembly"
//
// O bug: writeChunkLazy atualizava lazyMaxSeq ANTES de writeChunkFile. Se a
// escrita falhasse (disco cheio, inode esgotado, I/O error transitório), o seq
// ficava contabilizado como "máximo recebido" sem entrada no mapa pendingChunks.
// O finalizeLazy() então iterava de 0..lazyMaxSeq e falhava ao não encontrar o seq.
//
// Fix: lazyMaxSeq.Store() foi movido para APÓS a confirmação de escrita e inserção
// no mapa, garantindo invariante: ∀ seq ≤ lazyMaxSeq → pendingChunks[seq] existe.
func TestChunkAssembler_LazyMode_WriteFailure_LazyMaxSeqNotCorrupted(t *testing.T) {
	tmpDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ca, err := NewChunkAssemblerWithOptions("test-lazy-write-fail", tmpDir, logger, ChunkAssemblerOptions{
		Mode: AssemblerModeLazy,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer ca.Cleanup()

	// Escreve chunks 0 e 1 com sucesso.
	if err := ca.WriteChunk(0, bytes.NewReader([]byte("AA")), 2); err != nil {
		t.Fatalf("WriteChunk(0): %v", err)
	}
	if err := ca.WriteChunk(1, bytes.NewReader([]byte("BB")), 2); err != nil {
		t.Fatalf("WriteChunk(1): %v", err)
	}

	// Simula falha de I/O para o chunk 2 (seq mais alto) — análogo a disco cheio
	// ou inode esgotado em produção durante um backup de 16h com 12 streams.
	// O writeChunkFile interno vai devolver erro porque o diretório não existe.
	//
	// Para forçar a falha: removemos o diretório de shard que seria criado para seq 2.
	// seq=2 → shard "02" (2%256 = 2 → "02")
	shardDir := filepath.Join(ca.ChunkDir(), "02")
	if err := os.MkdirAll(shardDir, 0755); err != nil {
		t.Fatalf("pre-creating shard dir: %v", err)
	}
	// Torna o diretório de shard não-gravável para forçar falha em os.Create.
	if err := os.Chmod(shardDir, 0555); err != nil {
		t.Fatalf("chmod shard dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(shardDir, 0755) })

	// WriteChunk(2) deve falhar por não conseguir criar o arquivo de chunk.
	writeErr := ca.WriteChunk(2, bytes.NewReader([]byte("CC")), 2)
	if writeErr == nil {
		// Pode acontecer se o teste rodar como root — neste caso pulamos a verificação
		// de lazyMaxSeq pois a escrita "funcionou" sem falha.
		t.Skip("WriteChunk(2) não falhou (possivelmente rodando como root) — teste impossível")
	}

	// INVARIANTE CRÍTICO: lazyMaxSeq NÃO deve ter sido atualizado para seq=2,
	// pois a escrita falhou antes do registro no mapa.
	// Antes do fix, lazyMaxSeq seria 2, causando "missing chunk seq 2" no Finalize.
	gotMax := ca.lazyMaxSeq.Load()
	if gotMax >= 2 {
		t.Errorf("BUG DETECTADO: lazyMaxSeq=%d após falha de escrita do seq 2 — "+
			"isso causaria 'missing chunk seq 2 in lazy assembly' no Finalize()", gotMax)
	}

	// Chunks 0 e 1 ainda estão registrados — Finalize deve funcionar normalmente.
	resultPath, totalBytes, err := ca.Finalize()
	if err != nil {
		t.Fatalf("Finalize após falha de escrita: %v", err)
	}
	defer os.Remove(resultPath)

	if totalBytes != 4 {
		t.Errorf("expected totalBytes=4, got %d", totalBytes)
	}
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading assembled file: %v", err)
	}
	if string(content) != "AABB" {
		t.Errorf("expected %q, got %q", "AABB", content)
	}
}
