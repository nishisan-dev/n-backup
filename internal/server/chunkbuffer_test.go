// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

// newBufTestLogger retorna um logger silencioso para testes.
func newBufTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- ChunkBuffer desabilitado ---

func TestChunkBuffer_Disabled(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "0", SizeRaw: 0}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cb != nil {
		t.Fatal("expected nil ChunkBuffer when size=0")
	}
}

func TestChunkBuffer_Disabled_EmptySize(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "", SizeRaw: 0}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cb != nil {
		t.Fatal("expected nil ChunkBuffer when size is empty")
	}
}

func TestChunkBuffer_NilEnabled(t *testing.T) {
	var cb *ChunkBuffer
	if cb.Enabled() {
		t.Fatal("nil ChunkBuffer should not be Enabled()")
	}
}

func TestChunkBuffer_NilStats(t *testing.T) {
	var cb *ChunkBuffer
	stats := cb.Stats()
	if stats.Enabled {
		t.Fatal("nil ChunkBuffer stats should have Enabled=false")
	}
}

// --- ChunkBuffer habilitado ---

func TestChunkBuffer_Enabled(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "64mb", SizeRaw: 64 * 1024 * 1024, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cb == nil {
		t.Fatal("expected non-nil ChunkBuffer when SizeRaw=64MB")
	}
	if !cb.Enabled() {
		t.Fatal("ChunkBuffer should be Enabled()")
	}
	if cb.drainMode != "lazy" {
		t.Fatalf("expected drainMode=lazy, got %q", cb.drainMode)
	}
}

func TestChunkBuffer_Capacity(t *testing.T) {
	const size = 10 * 1024 * 1024 // 10 MB → 10 slots (avgSlotSize=1MB)
	cfg := config.ChunkBufferConfig{SizeRaw: size, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cb == nil {
		t.Fatal("expected non-nil ChunkBuffer")
	}
	if cap(cb.slots) != 10 {
		t.Errorf("expected capacity=10, got %d", cap(cb.slots))
	}
}

func TestChunkBuffer_MinCapacity(t *testing.T) {
	// Tamanho menor que 1 slot → mínimo de 2 slots
	cfg := config.ChunkBufferConfig{SizeRaw: 512 * 1024, DrainMode: "lazy"} // 512KB
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cap(cb.slots) < 2 {
		t.Errorf("expected capacity >= 2, got %d", cap(cb.slots))
	}
}

// --- Push e Drain (lazy) ---

func TestChunkBuffer_Push_And_Drain_Lazy(t *testing.T) {
	tmpDir := t.TempDir()
	logger := newBufTestLogger()

	assembler, err := NewChunkAssembler("buf-drain-lazy", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer assembler.Cleanup()

	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	chunks := []struct {
		seq  uint32
		data string
	}{{0, "HELLO"}, {1, "WORLD"}, {2, "!!!!"}}
	for _, c := range chunks {
		if err := cb.Push(c.seq, []byte(c.data), assembler); err != nil {
			t.Fatalf("Push(%d): %v", c.seq, err)
		}
	}

	if !bufWaitDrained(cb, 3, 2*time.Second) {
		t.Fatalf("chunks not drained in time: pushed=%d drained=%d",
			cb.totalPushed.Load(), cb.totalDrained.Load())
	}

	_, totalBytes, err := assembler.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	expected := int64(len("HELLOWORLD!!!!"))
	if totalBytes != expected {
		t.Errorf("expected totalBytes=%d, got %d", expected, totalBytes)
	}
}

// --- Push e Drain (eager) ---

func TestChunkBuffer_Push_And_Drain_Eager(t *testing.T) {
	tmpDir := t.TempDir()
	logger := newBufTestLogger()

	assembler, err := NewChunkAssemblerWithOptions("buf-drain-eager", tmpDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: defaultPendingMemLimit,
	})
	if err != nil {
		t.Fatalf("NewChunkAssemblerWithOptions: %v", err)
	}
	defer assembler.Cleanup()

	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainMode: "eager"}
	cb := NewChunkBuffer(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i, s := range []string{"AA", "BB", "CC"} {
		if err := cb.Push(uint32(i), []byte(s), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	if !bufWaitDrained(cb, 3, 2*time.Second) {
		t.Fatalf("chunks not drained: pushed=%d drained=%d",
			cb.totalPushed.Load(), cb.totalDrained.Load())
	}

	_, totalBytes, err := assembler.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if totalBytes != 6 {
		t.Errorf("expected totalBytes=6, got %d", totalBytes)
	}
}

// --- Stats ---

func TestChunkBuffer_Stats(t *testing.T) {
	tmpDir := t.TempDir()
	logger := newBufTestLogger()

	assembler, err := NewChunkAssembler("buf-stats", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer assembler.Cleanup()

	cfg := config.ChunkBufferConfig{SizeRaw: 32 * 1024 * 1024, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 2; i++ {
		if err := cb.Push(uint32(i), []byte("TEST"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	if !bufWaitDrained(cb, 2, 2*time.Second) {
		t.Fatalf("not drained in time")
	}

	stats := cb.Stats()
	if !stats.Enabled {
		t.Error("expected Enabled=true")
	}
	if stats.TotalPushed != 2 {
		t.Errorf("expected TotalPushed=2, got %d", stats.TotalPushed)
	}
	if stats.TotalDrained != 2 {
		t.Errorf("expected TotalDrained=2, got %d", stats.TotalDrained)
	}
	if stats.BackpressureEvents != 0 {
		t.Errorf("expected BackpressureEvents=0, got %d", stats.BackpressureEvents)
	}
	if stats.DrainMode != "lazy" {
		t.Errorf("expected DrainMode=lazy, got %q", stats.DrainMode)
	}
	if stats.Capacity != cap(cb.slots) {
		t.Errorf("expected Capacity=%d, got %d", cap(cb.slots), stats.Capacity)
	}
}

// --- Backpressure ---

func TestChunkBuffer_Backpressure(t *testing.T) {
	logger := newBufTestLogger()
	tmpDir := t.TempDir()

	// O buffer tem no mínimo 2 slots (guarda da capacidade mínima).
	// Usamos SizeRaw = 1MB → 1 slot calculado → ajustado para 2 slots mínimos.
	// Sem drainer, preenchemos os 2 slots e o 3º Push deve bloquear e retornar erro de backpressure.
	cfg := config.ChunkBufferConfig{SizeRaw: 1 * 1024 * 1024, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, logger)

	// Garante que a capacidade mínima aplicada foi 2
	if cap(cb.slots) < 2 {
		t.Skipf("unexpected capacity %d, skipping", cap(cb.slots))
	}

	assembler, err := NewChunkAssembler("buf-bp", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer assembler.Cleanup()

	data := make([]byte, 64)

	// Preenche todos os slots sem drainer
	for i := 0; i < cap(cb.slots); i++ {
		if err := cb.Push(uint32(i), data, assembler); err != nil {
			t.Fatalf("Push(%d) should succeed on empty slot: %v", i, err)
		}
	}

	// Push extra deve falhar por timeout de backpressure (canal cheio, sem drainer)
	done := make(chan error, 1)
	go func() { done <- cb.Push(uint32(cap(cb.slots)), data, assembler) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected backpressure error when buffer is full")
		}
		if cb.backpressureEvents.Load() != 1 {
			t.Errorf("expected 1 backpressure event, got %d", cb.backpressureEvents.Load())
		}
	case <-time.After(chunkBufferPushTimeout + 2*time.Second):
		t.Fatal("Push did not return after timeout")
	}
}

// --- Concorrência ---

func TestChunkBuffer_ConcurrentPush(t *testing.T) {
	tmpDir := t.TempDir()
	logger := newBufTestLogger()

	assembler, err := NewChunkAssembler("buf-concurrent", tmpDir, logger)
	if err != nil {
		t.Fatalf("NewChunkAssembler: %v", err)
	}
	defer assembler.Cleanup()

	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainMode: "lazy"}
	cb := NewChunkBuffer(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			if err := cb.Push(uint32(seq), []byte("DATA"), assembler); err != nil {
				t.Errorf("Push(%d): %v", seq, err)
			}
		}(i)
	}
	wg.Wait()

	if !bufWaitDrained(cb, n, 3*time.Second) {
		t.Fatalf("concurrent chunks not drained: pushed=%d drained=%d",
			cb.totalPushed.Load(), cb.totalDrained.Load())
	}
}

// --- Helpers ---

// bufWaitDrained aguarda até que totalDrained >= n dentro de timeout.
func bufWaitDrained(cb *ChunkBuffer, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cb.totalDrained.Load() >= int64(n) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
