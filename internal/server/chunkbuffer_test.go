// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bytes"
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

// newBufAssembler cria um assembler eager para testes do buffer.
func newBufAssembler(t *testing.T, sessionID string) *ChunkAssembler {
	t.Helper()
	a, err := NewChunkAssembler(sessionID, t.TempDir(), newBufTestLogger())
	if err != nil {
		t.Fatalf("NewChunkAssembler(%s): %v", sessionID, err)
	}
	t.Cleanup(func() { a.Cleanup() })
	return a
}

// --- Desabilitado ---

func TestChunkBuffer_Disabled(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "0", SizeRaw: 0}
	if NewChunkBuffer(cfg, newBufTestLogger()) != nil {
		t.Fatal("expected nil ChunkBuffer when size=0")
	}
}

func TestChunkBuffer_Disabled_EmptySize(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "", SizeRaw: 0}
	if NewChunkBuffer(cfg, newBufTestLogger()) != nil {
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
	if cb.Stats().Enabled {
		t.Fatal("nil ChunkBuffer stats should have Enabled=false")
	}
}

func TestChunkBuffer_NilFlush(t *testing.T) {
	var cb *ChunkBuffer
	if err := cb.Flush(); err != nil {
		t.Fatalf("nil ChunkBuffer Flush should be no-op, got: %v", err)
	}
}

// --- Habilitado ---

func TestChunkBuffer_Enabled(t *testing.T) {
	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	if cb == nil {
		t.Fatal("expected non-nil ChunkBuffer")
	}
	if !cb.Enabled() {
		t.Fatal("ChunkBuffer should be Enabled()")
	}
	if cb.drainRatio != 0.5 {
		t.Fatalf("expected drainRatio=0.5, got %v", cb.drainRatio)
	}
}

func TestChunkBuffer_CapacityBytes(t *testing.T) {
	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	stats := cb.Stats()
	if stats.CapacityBytes != 64*1024*1024 {
		t.Errorf("expected CapacityBytes=64MB, got %d", stats.CapacityBytes)
	}
	if !stats.Enabled {
		t.Error("expected Enabled=true")
	}
}

// --- Push e Drain (ratio=0 write-through) ---

func TestChunkBuffer_Push_And_Drain_WriteThrough(t *testing.T) {
	assembler := newBufAssembler(t, "buf-wt")
	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainRatio: 0.0}
	cb := NewChunkBuffer(cfg, newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i, s := range []string{"HELLO", "WORLD", "!!!!"} {
		data := []byte(s)
		if err := cb.Push(uint32(i), data, assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	if !bufWaitDrained(cb, 3, 2*time.Second) {
		t.Fatalf("not drained: pushed=%d drained=%d", cb.totalPushed.Load(), cb.totalDrained.Load())
	}

	_, totalBytes, err := assembler.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if totalBytes != int64(len("HELLOWORLD!!!!")) {
		t.Errorf("expected %d bytes, got %d", len("HELLOWORLD!!!!"), totalBytes)
	}
}

// --- Push e Drain (ratio=0.5) ---

func TestChunkBuffer_Push_And_Drain_Ratio(t *testing.T) {
	const sizeRaw = 10 * 1024 * 1024 // 10MB
	assembler := newBufAssembler(t, "buf-ratio")
	cfg := config.ChunkBufferConfig{SizeRaw: sizeRaw, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 5; i++ {
		if err := cb.Push(uint32(i), []byte("AB"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	// Chunks de 2 bytes num buffer de 10MB nunca atingem o threshold de 0.5.
	// Flush() força o drain — exatamente o que ocorre em produção antes do Finalize().
	if err := cb.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if cb.totalDrained.Load() != 5 {
		t.Errorf("expected 5 drained after Flush, got %d", cb.totalDrained.Load())
	}
}

// --- Fallback: chunk maior que capacidade disponível ---

func TestChunkBuffer_Fallback_ChunkExceedsCapacity(t *testing.T) {
	const sizeRaw = 4 * 1024 // buffer minúsculo (4KB)
	assembler := newBufAssembler(t, "buf-fallback")
	cfg := config.ChunkBufferConfig{SizeRaw: sizeRaw, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	// Chunk de 8KB > 4KB de capacidade → deve acionar fallback direto no assembler
	bigChunk := bytes.Repeat([]byte("X"), 8*1024)
	if err := cb.Push(0, bigChunk, assembler); err != nil {
		t.Fatalf("Push com fallback não deve retornar erro: %v", err)
	}

	if cb.totalFallbacks.Load() != 1 {
		t.Errorf("expected 1 fallback, got %d", cb.totalFallbacks.Load())
	}
	// Push via fallback não incrementa totalPushed (foi direto ao assembler)
	if cb.totalPushed.Load() != 0 {
		t.Errorf("fallback não deveria incrementar totalPushed, got %d", cb.totalPushed.Load())
	}

	_, totalBytes, err := assembler.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if totalBytes != int64(len(bigChunk)) {
		t.Errorf("expected %d bytes after fallback, got %d", len(bigChunk), totalBytes)
	}
}

// --- Stats ---

func TestChunkBuffer_Stats(t *testing.T) {
	assembler := newBufAssembler(t, "buf-stats")
	cfg := config.ChunkBufferConfig{SizeRaw: 32 * 1024 * 1024, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 2; i++ {
		if err := cb.Push(uint32(i), []byte("TEST"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	// Flush() força o drain (chunks pequenos não atingem threshold de 0.5 em 32MB)
	if err := cb.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	s := cb.Stats()
	if !s.Enabled {
		t.Error("expected Enabled=true")
	}
	if s.TotalPushed != 2 {
		t.Errorf("expected TotalPushed=2, got %d", s.TotalPushed)
	}
	if s.TotalDrained != 2 {
		t.Errorf("expected TotalDrained=2, got %d", s.TotalDrained)
	}
	if s.BackpressureEvents != 0 {
		t.Errorf("expected BackpressureEvents=0, got %d", s.BackpressureEvents)
	}
	if s.DrainRatio != 0.5 {
		t.Errorf("expected DrainRatio=0.5, got %v", s.DrainRatio)
	}
	if s.CapacityBytes != 32*1024*1024 {
		t.Errorf("expected CapacityBytes=32MB, got %d", s.CapacityBytes)
	}
	if s.InFlightBytes != 0 {
		t.Errorf("expected InFlightBytes=0 after drain, got %d", s.InFlightBytes)
	}
}

// --- FillRatio ---

func TestChunkBuffer_FillRatio(t *testing.T) {
	const sizeRaw = 4 * 1024 * 1024                                    // 4MB
	cfg := config.ChunkBufferConfig{SizeRaw: sizeRaw, DrainRatio: 1.0} // só drena quando cheio
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	// não inicia drainer — testa apenas o tracking de bytes

	assembler := newBufAssembler(t, "buf-fillratio")

	// Push de 1MB → fill ratio = 0.25
	data := make([]byte, 1*1024*1024)
	if err := cb.Push(0, data, assembler); err != nil {
		t.Fatalf("Push: %v", err)
	}

	s := cb.Stats()
	if s.InFlightBytes != int64(len(data)) {
		t.Errorf("expected InFlightBytes=%d, got %d", len(data), s.InFlightBytes)
	}
	expectedRatio := float64(len(data)) / float64(sizeRaw)
	if s.FillRatio < expectedRatio-0.01 || s.FillRatio > expectedRatio+0.01 {
		t.Errorf("expected FillRatio~%.2f, got %.2f", expectedRatio, s.FillRatio)
	}
}

// --- Flush ---

func TestChunkBuffer_Flush_WaitsForDrain(t *testing.T) {
	assembler := newBufAssembler(t, "buf-flush")
	cfg := config.ChunkBufferConfig{SizeRaw: 32 * 1024 * 1024, DrainRatio: 1.0} // drain manual via Flush
	cb := NewChunkBuffer(cfg, newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 3; i++ {
		if err := cb.Push(uint32(i), []byte("FLUSH_TEST"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}

	// Flush deve aguardar e forçar drenagem total
	if err := cb.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if cb.inFlightBytes.Load() != 0 {
		t.Errorf("Flush returned but inFlightBytes=%d", cb.inFlightBytes.Load())
	}
}

// --- Backpressure ---

func TestChunkBuffer_Backpressure(t *testing.T) {
	cfg := config.ChunkBufferConfig{SizeRaw: 1 * 1024 * 1024, DrainRatio: 0.5}
	cb := NewChunkBuffer(cfg, newBufTestLogger())
	assembler := newBufAssembler(t, "buf-bp")

	// Garante que temos pelo menos 2 slots (mínimo aplicado)
	if cap(cb.slots) < 2 {
		t.Skipf("unexpected capacity %d, skipping", cap(cb.slots))
	}

	data := make([]byte, 64)

	// Preenche todos os slots sem drainer
	for i := 0; i < cap(cb.slots); i++ {
		if err := cb.Push(uint32(i), data, assembler); err != nil {
			t.Fatalf("Push(%d) deveria funcionar: %v", i, err)
		}
	}

	// Push extra deve falhar por timeout de backpressure
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
	assembler := newBufAssembler(t, "buf-concurrent")
	cfg := config.ChunkBufferConfig{SizeRaw: 64 * 1024 * 1024, DrainRatio: 0.0}
	cb := NewChunkBuffer(cfg, newBufTestLogger())

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
		t.Fatalf("concurrent not drained: pushed=%d drained=%d",
			cb.totalPushed.Load(), cb.totalDrained.Load())
	}
}

// --- Helpers ---

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
