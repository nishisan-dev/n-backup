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

// pFloat64 retorna um ponteiro para um valor float64 literal.
// Helper necessário porque config.ChunkBufferConfig.DrainRatio é *float64.
func pFloat64(v float64) *float64 { return &v }

// newBufAssembler cria um ChunkAssembler eager (default) para testes do buffer.
func newBufAssembler(t *testing.T, sessionID string) *ChunkAssembler {
	t.Helper()
	a, err := NewChunkAssembler(sessionID, t.TempDir(), newBufTestLogger())
	if err != nil {
		t.Fatalf("NewChunkAssembler(%s): %v", sessionID, err)
	}
	t.Cleanup(func() { a.Cleanup() })
	return a
}

// newBufConfig cria uma ChunkBufferConfig com SizeRaw e DrainRatioRaw preenchidos.
func newBufConfig(sizeRaw int64, drainRatio float64) config.ChunkBufferConfig {
	return config.ChunkBufferConfig{
		SizeRaw:       sizeRaw,
		DrainRatioRaw: drainRatio,
		DrainRatio:    pFloat64(drainRatio),
	}
}

// --- Desabilitado ---

func TestChunkBuffer_Disabled(t *testing.T) {
	cfg := config.ChunkBufferConfig{Size: "0", SizeRaw: 0}
	if NewChunkBuffer(cfg, newBufTestLogger()) != nil {
		t.Fatal("expected nil ChunkBuffer when size=0")
	}
}

func TestChunkBuffer_Disabled_EmptySize(t *testing.T) {
	if NewChunkBuffer(config.ChunkBufferConfig{}, newBufTestLogger()) != nil {
		t.Fatal("expected nil ChunkBuffer when SizeRaw is 0")
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
	if err := cb.Flush(nil); err != nil {
		t.Fatalf("nil ChunkBuffer Flush should be no-op, got: %v", err)
	}
}

// --- Habilitado ---

func TestChunkBuffer_Enabled(t *testing.T) {
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.5), newBufTestLogger())
	if cb == nil || !cb.Enabled() {
		t.Fatal("expected enabled non-nil ChunkBuffer")
	}
	if cb.drainRatio != 0.5 {
		t.Fatalf("expected drainRatio=0.5, got %v", cb.drainRatio)
	}
}

func TestChunkBuffer_CapacityBytes(t *testing.T) {
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.5), newBufTestLogger())
	s := cb.Stats()
	if s.CapacityBytes != 64*1024*1024 {
		t.Errorf("expected CapacityBytes=64MB, got %d", s.CapacityBytes)
	}
}

// --- Push e Drain (write-through, ratio=0.0) ---

func TestChunkBuffer_Push_And_Drain_WriteThrough(t *testing.T) {
	assembler := newBufAssembler(t, "buf-wt")
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.0), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i, s := range []string{"HELLO", "WORLD", "!!!!"} {
		if err := cb.Push(uint32(i), []byte(s), assembler); err != nil {
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

// --- Push e Drain (ratio=0.5, Flush forçado) ---

func TestChunkBuffer_Push_And_Drain_Ratio(t *testing.T) {
	assembler := newBufAssembler(t, "buf-ratio")
	cb := NewChunkBuffer(newBufConfig(10*1024*1024, 0.5), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 5; i++ {
		if err := cb.Push(uint32(i), []byte("AB"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	// Chunks de 2 bytes em 10MB nunca atingem threshold=0.5 por si só.
	// Flush() força o drain — exatamente o que ocorre antes de Finalize() em produção.
	if err := cb.Flush(assembler); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if cb.totalDrained.Load() != 5 {
		t.Errorf("expected 5 drained after Flush, got %d", cb.totalDrained.Load())
	}
}

// --- Fallback: chunk maior que capacidade disponível ---

func TestChunkBuffer_Fallback_ChunkExceedsCapacity(t *testing.T) {
	assembler := newBufAssembler(t, "buf-fallback")
	cb := NewChunkBuffer(newBufConfig(4*1024, 0.5), newBufTestLogger()) // buffer 4KB

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	bigChunk := bytes.Repeat([]byte("X"), 8*1024) // 8KB > 4KB → fallback
	if err := cb.Push(0, bigChunk, assembler); err != nil {
		t.Fatalf("Push com fallback não deve retornar erro: %v", err)
	}

	if cb.totalFallbacks.Load() != 1 {
		t.Errorf("expected 1 fallback, got %d", cb.totalFallbacks.Load())
	}
	if cb.totalPushed.Load() != 0 {
		t.Errorf("fallback não deve incrementar totalPushed, got %d", cb.totalPushed.Load())
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
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 0.5), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 2; i++ {
		if err := cb.Push(uint32(i), []byte("TEST"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	if err := cb.Flush(assembler); err != nil {
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
	if s.InFlightBytes != 0 {
		t.Errorf("expected InFlightBytes=0 after flush, got %d", s.InFlightBytes)
	}
}

// --- FillRatio ---

func TestChunkBuffer_FillRatio(t *testing.T) {
	const sizeRaw = 4 * 1024 * 1024 // 4MB
	cb := NewChunkBuffer(newBufConfig(sizeRaw, 1.0), newBufTestLogger())
	// sem drainer — testa apenas o tracking de bytes

	assembler := newBufAssembler(t, "buf-fillratio")
	data := make([]byte, 1*1024*1024) // 1MB → fill = 0.25
	if err := cb.Push(0, data, assembler); err != nil {
		t.Fatalf("Push: %v", err)
	}

	s := cb.Stats()
	if s.InFlightBytes != int64(len(data)) {
		t.Errorf("expected InFlightBytes=%d, got %d", len(data), s.InFlightBytes)
	}
	expected := float64(len(data)) / float64(sizeRaw)
	if s.FillRatio < expected-0.01 || s.FillRatio > expected+0.01 {
		t.Errorf("expected FillRatio~%.4f, got %.4f", expected, s.FillRatio)
	}
}

// --- Flush scoped por sessão (FIX #1) ---

func TestChunkBuffer_Flush_WaitsForDrain(t *testing.T) {
	assembler := newBufAssembler(t, "buf-flush")
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 1.0), newBufTestLogger()) // drain manual

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	for i := 0; i < 3; i++ {
		if err := cb.Push(uint32(i), []byte("FLUSH_TEST"), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	if err := cb.Flush(assembler); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if cb.inFlightBytes.Load() != 0 {
		t.Errorf("Flush returned but inFlightBytes=%d", cb.inFlightBytes.Load())
	}
}

func TestChunkBuffer_Flush_NoPending_DoesNotCreateSessionEntry(t *testing.T) {
	assembler := newBufAssembler(t, "buf-flush-empty")
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 1.0), newBufTestLogger())

	if got := countSessionEntries(cb); got != 0 {
		t.Fatalf("expected 0 session entries before Flush, got %d", got)
	}

	if err := cb.Flush(assembler); err != nil {
		t.Fatalf("Flush on empty session should be no-op, got: %v", err)
	}

	if got := countSessionEntries(cb); got != 0 {
		t.Fatalf("expected 0 session entries after empty Flush, got %d", got)
	}
}

func TestChunkBuffer_Flush_ZeroCounter_CleansSessionEntry(t *testing.T) {
	assembler := newBufAssembler(t, "buf-flush-zero")
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 1.0), newBufTestLogger())

	// Simula uma entrada residual zerada no mapa (cenário de vazamento).
	cb.getSessionCounter(assembler)
	if got := countSessionEntries(cb); got != 1 {
		t.Fatalf("expected 1 session entry before Flush, got %d", got)
	}

	if err := cb.Flush(assembler); err != nil {
		t.Fatalf("Flush should clean zero counter entry, got: %v", err)
	}

	if got := countSessionEntries(cb); got != 0 {
		t.Fatalf("expected 0 session entries after Flush cleanup, got %d", got)
	}
}

// TestChunkBuffer_Flush_NotBlockedByOtherSession verifica FIX #1:
// Flush da sessão A não deve bloquear enquanto a sessão B ainda está enviando chunks.
func TestChunkBuffer_Flush_NotBlockedByOtherSession(t *testing.T) {
	assemblerA := newBufAssembler(t, "buf-session-A")
	assemblerB := newBufAssembler(t, "buf-session-B")
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.0), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	// Envia 3 chunks da sessão A e drena completamente.
	for i := 0; i < 3; i++ {
		if err := cb.Push(uint32(i), []byte("SESSION_A"), assemblerA); err != nil {
			t.Fatalf("Push A(%d): %v", i, err)
		}
	}

	// Envia 1 chunk da sessão B sem drenar (simula sessão ainda ativa).
	// O drainer vai processar, mas o teste verifica que Flush(A) não aguarda B.
	if err := cb.Push(0, []byte("SESSION_B"), assemblerB); err != nil {
		t.Fatalf("Push B: %v", err)
	}

	// Flush(A) deve retornar rapidamente — não deve esperar bytes da sessão B.
	flushDone := make(chan error, 1)
	go func() { flushDone <- cb.Flush(assemblerA) }()

	select {
	case err := <-flushDone:
		if err != nil {
			t.Errorf("Flush(A) should succeed independently of session B: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Flush(A) bloqueou esperando por sessão B — FIX #1 not working")
	}
}

// --- Race condition: reserva de bytes via CAS (FIX #2) ---

func TestChunkBuffer_CAS_NoRaceOnCapacity(t *testing.T) {
	// Usa o race detector do go test (-race) para detectar acessos concorrentes.
	assembler := newBufAssembler(t, "buf-cas")
	const sizeRaw = 64 * 1024 * 1024
	cb := NewChunkBuffer(newBufConfig(sizeRaw, 0.0), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			// Cada goroutine tenta fazer push concorrentemente — CAS evita race.
			_ = cb.Push(uint32(seq), make([]byte, 512*1024), assembler) // 512KB cada
		}(i)
	}
	wg.Wait()

	if err := cb.Flush(assembler); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Invariante: inFlightBytes nunca deve exceder sizeRaw (o CAS evita overflow).
	s := cb.Stats()
	if s.InFlightBytes > sizeRaw {
		t.Errorf("inFlightBytes=%d exceeded sizeRaw=%d: CAS race condition", s.InFlightBytes, sizeRaw)
	}
}

// --- Backpressure ---

func TestChunkBuffer_Backpressure(t *testing.T) {
	cb := NewChunkBuffer(newBufConfig(1*1024*1024, 0.5), newBufTestLogger())
	assembler := newBufAssembler(t, "buf-bp")

	if cap(cb.slots) < 2 {
		t.Skipf("unexpected capacity %d", cap(cb.slots))
	}

	data := make([]byte, 64)
	for i := 0; i < cap(cb.slots); i++ {
		if err := cb.Push(uint32(i), data, assembler); err != nil {
			t.Fatalf("Push(%d) deveria funcionar: %v", i, err)
		}
	}

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
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.0), newBufTestLogger())

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
		t.Fatalf("not drained: pushed=%d drained=%d",
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

func countSessionEntries(cb *ChunkBuffer) int {
	var n int
	cb.sessionBytes.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// --- SessionBytes ---

func TestChunkBuffer_SessionBytes_Nil(t *testing.T) {
	var cb *ChunkBuffer
	if got := cb.SessionBytes(nil); got != 0 {
		t.Errorf("nil ChunkBuffer.SessionBytes should return 0, got %d", got)
	}
}

func TestChunkBuffer_SessionBytes_NoEntry(t *testing.T) {
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 1.0), newBufTestLogger())
	assembler := newBufAssembler(t, "sb-noentry")
	// Nenhum push foi feito — SessionBytes deve retornar 0 sem criar entrada.
	if got := cb.SessionBytes(assembler); got != 0 {
		t.Errorf("expected SessionBytes=0 before any push, got %d", got)
	}
	if n := countSessionEntries(cb); n != 0 {
		t.Errorf("SessionBytes should not create session entry, got %d entries", n)
	}
}

func TestChunkBuffer_SessionBytes_AfterPush(t *testing.T) {
	assembler := newBufAssembler(t, "sb-push")
	cb := NewChunkBuffer(newBufConfig(32*1024*1024, 1.0), newBufTestLogger())
	// Sem drainer — chunk fica em voo.
	data := make([]byte, 4096)
	if err := cb.Push(0, data, assembler); err != nil {
		t.Fatalf("Push: %v", err)
	}
	got := cb.SessionBytes(assembler)
	if got != int64(len(data)) {
		t.Errorf("expected SessionBytes=%d, got %d", len(data), got)
	}
}

// --- DrainRateMBs ---

func TestChunkBuffer_DrainRateMBs_Nil(t *testing.T) {
	var cb *ChunkBuffer
	s := cb.Stats()
	if s.DrainRateMBs != 0 {
		t.Errorf("nil ChunkBuffer DrainRateMBs should be 0, got %f", s.DrainRateMBs)
	}
}

func TestChunkBuffer_DrainRateMBs_FirstCall(t *testing.T) {
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.0), newBufTestLogger())
	s := cb.Stats()
	// Primeira chamada sempre retorna 0 (snapshot ainda não construído).
	if s.DrainRateMBs != 0 {
		t.Errorf("first Stats() DrainRateMBs should be 0.0 (no snapshot yet), got %f", s.DrainRateMBs)
	}
}

func TestChunkBuffer_DrainRateMBs_AfterDrain(t *testing.T) {
	assembler := newBufAssembler(t, "drainrate")
	cb := NewChunkBuffer(newBufConfig(64*1024*1024, 0.0), newBufTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb.StartDrainer(ctx)

	// Primeira chamada inicializa snapshot.
	cb.Stats()

	for i := 0; i < 5; i++ {
		if err := cb.Push(uint32(i), make([]byte, 512*1024), assembler); err != nil {
			t.Fatalf("Push(%d): %v", i, err)
		}
	}
	if !bufWaitDrained(cb, 5, 3*time.Second) {
		t.Fatalf("not drained")
	}

	// Aguarda 1s para que a janela do snapshot se abra.
	time.Sleep(1100 * time.Millisecond)

	s := cb.Stats()
	// Não exigimos valor exato, apenas que a taxa seja >= 0 e tenha sido calculada.
	if s.DrainRateMBs < 0 {
		t.Errorf("DrainRateMBs should be >= 0, got %f", s.DrainRateMBs)
	}
	if s.TotalDrained != 5 {
		t.Errorf("expected 5 drained, got %d", s.TotalDrained)
	}
}
