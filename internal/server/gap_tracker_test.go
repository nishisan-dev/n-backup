// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"log/slog"
	"testing"
	"time"
)

func testGapLogger() *slog.Logger {
	return slog.Default()
}

func TestGapTracker_DetectsGapAfterTimeout(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 5, testGapLogger())

	// Recebe seq 0, 1, 3 (gap em 2)
	gt.RecordChunk(0)
	gt.RecordChunk(1)
	gt.RecordChunk(3)

	// Antes do timeout, CheckGaps deve retornar vazio
	gaps := gt.CheckGaps()
	if len(gaps) != 0 {
		t.Fatalf("expected 0 gaps before timeout, got %d: %v", len(gaps), gaps)
	}

	// Espera timeout
	time.Sleep(60 * time.Millisecond)

	// Agora deve reportar gap seq=2
	gaps = gt.CheckGaps()
	if len(gaps) != 1 || gaps[0] != 2 {
		t.Fatalf("expected gap [2], got %v", gaps)
	}
}

func TestGapTracker_TransientGapResolved(t *testing.T) {
	gt := NewGapTracker("test-session", 100*time.Millisecond, 5, testGapLogger())

	// Recebe seq 0, 1, 3 (gap em 2)
	gt.RecordChunk(0)
	gt.RecordChunk(1)
	gt.RecordChunk(3)

	// Gap transiente: seq 2 chega antes do timeout
	gt.RecordChunk(2)

	// Espera timeout
	time.Sleep(110 * time.Millisecond)

	// Deve retornar vazio (gap resolvido)
	gaps := gt.CheckGaps()
	if len(gaps) != 0 {
		t.Fatalf("expected 0 gaps after resolution, got %d: %v", len(gaps), gaps)
	}
}

func TestGapTracker_MultipleGaps(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 10, testGapLogger())

	// Recebe seq 0 e 5 — gaps em 1, 2, 3, 4
	gt.RecordChunk(0)
	gt.RecordChunk(5)

	time.Sleep(60 * time.Millisecond)

	gaps := gt.CheckGaps()
	if len(gaps) != 4 {
		t.Fatalf("expected 4 gaps, got %d: %v", len(gaps), gaps)
	}

	// Verifica que os gaps são 1, 2, 3, 4
	gapSet := make(map[uint32]bool)
	for _, g := range gaps {
		gapSet[g] = true
	}
	for seq := uint32(1); seq <= 4; seq++ {
		if !gapSet[seq] {
			t.Errorf("expected gap seq %d to be reported", seq)
		}
	}
}

func TestGapTracker_MaxNACKsPerCycle(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 3, testGapLogger())

	// 10 gaps: recebe seq 0 e 11
	gt.RecordChunk(0)
	gt.RecordChunk(11)

	time.Sleep(60 * time.Millisecond)

	// Deve retornar no máximo 3
	gaps := gt.CheckGaps()
	if len(gaps) != 3 {
		t.Fatalf("expected 3 gaps (maxNACKsPerCycle), got %d: %v", len(gaps), gaps)
	}
}

func TestGapTracker_NoDuplicateNACKs(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 5, testGapLogger())

	gt.RecordChunk(0)
	gt.RecordChunk(3) // gaps 1, 2

	time.Sleep(60 * time.Millisecond)

	// Primeira chamada: retorna gaps 1, 2
	gaps1 := gt.CheckGaps()
	if len(gaps1) != 2 {
		t.Fatalf("expected 2 gaps on first check, got %d", len(gaps1))
	}

	// Segunda chamada: gaps já notificados, deve retornar vazio
	gaps2 := gt.CheckGaps()
	if len(gaps2) != 0 {
		t.Fatalf("expected 0 gaps on second check (already notified), got %d: %v", len(gaps2), gaps2)
	}
}

func TestGapTracker_ResolveGap(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 5, testGapLogger())

	gt.RecordChunk(0)
	gt.RecordChunk(3) // gaps 1, 2

	time.Sleep(60 * time.Millisecond)

	// CheckGaps notifica gaps
	gt.CheckGaps()

	// Resolve gap 1 via retransmissão
	gt.ResolveGap(1)

	// PendingGaps deve ser 1 (apenas seq 2)
	pending := gt.PendingGaps()
	if pending != 1 {
		t.Fatalf("expected 1 pending gap after resolving 1, got %d", pending)
	}
}

func TestGapTracker_PendingGaps(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 5, testGapLogger())

	gt.RecordChunk(0)
	gt.RecordChunk(5) // gaps 1, 2, 3, 4

	if gt.PendingGaps() != 4 {
		t.Fatalf("expected 4 pending gaps, got %d", gt.PendingGaps())
	}

	gt.RecordChunk(2) // resolve gap 2

	if gt.PendingGaps() != 3 {
		t.Fatalf("expected 3 pending gaps after receiving seq 2, got %d", gt.PendingGaps())
	}
}

func TestGapTracker_SequentialChunks_NoGaps(t *testing.T) {
	gt := NewGapTracker("test-session", 50*time.Millisecond, 5, testGapLogger())

	// Recebe chunks sequenciais — nenhum gap deve ser criado
	for i := uint32(0); i < 100; i++ {
		gt.RecordChunk(i)
	}

	time.Sleep(60 * time.Millisecond)

	gaps := gt.CheckGaps()
	if len(gaps) != 0 {
		t.Fatalf("expected 0 gaps for sequential chunks, got %d: %v", len(gaps), gaps)
	}

	if gt.PendingGaps() != 0 {
		t.Fatalf("expected 0 pending gaps, got %d", gt.PendingGaps())
	}

	if gt.MaxSeenSeq() != 99 {
		t.Fatalf("expected maxSeenSeq=99, got %d", gt.MaxSeenSeq())
	}
}
