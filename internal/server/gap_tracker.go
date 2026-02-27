// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// GapTracker detecta chunks faltantes em sessões paralelas de forma proativa.
// Mantém um mapa de globalSeqs recebidos e detecta gaps persistentes quando
// um seq N+2 chega mas N+1 não — e esse gap persiste além do timeout configurado.
//
// Gaps transientes (chunks fora de ordem que chegam em poucos segundos) são tolerados.
// Apenas gaps que persistem além do timeout são reportados para envio de NACK.
type GapTracker struct {
	sessionID string

	// received marca quais globalSeqs já foram recebidos pelo assembler.
	received map[uint32]bool

	// maxSeenSeq é o maior globalSeq visto até agora.
	maxSeenSeq uint32

	// hasSeenSeq evita ambiguidade com o valor zero de maxSeenSeq.
	// Sem isso, um primeiro chunk fora de ordem (ex: seq 1) não detectaria o gap seq 0.
	hasSeenSeq bool

	// firstSeen armazena quando cada gap foi detectado pela primeira vez.
	// Usado para tolerância de gaps transientes (out-of-order).
	firstSeen map[uint32]time.Time

	// notifiedGaps rastreia gaps já notificados via NACK, evitando duplicatas.
	notifiedGaps map[uint32]bool

	// gapTimeout é o tempo mínimo que um gap deve persistir antes de ser reportado.
	// Default: 2x streamReadDeadline (60s) para tolerar reconexões normais.
	gapTimeout time.Duration

	// maxNACKsPerCycle limita quantos gaps são reportados por chamada de CheckGaps,
	// evitando flood de NACKs em degradação severa.
	maxNACKsPerCycle int

	mu     sync.Mutex
	logger *slog.Logger
}

// NewGapTracker cria um GapTracker para uma sessão paralela.
func NewGapTracker(sessionID string, gapTimeout time.Duration, maxNACKsPerCycle int, logger *slog.Logger) *GapTracker {
	if maxNACKsPerCycle <= 0 {
		maxNACKsPerCycle = 5
	}
	return &GapTracker{
		sessionID:        sessionID,
		received:         make(map[uint32]bool),
		firstSeen:        make(map[uint32]time.Time),
		notifiedGaps:     make(map[uint32]bool),
		gapTimeout:       gapTimeout,
		maxNACKsPerCycle: maxNACKsPerCycle,
		logger:           logger,
	}
}

// RecordChunk registra que o globalSeq foi recebido com sucesso.
// Deve ser chamado pelo receiveParallelStream a cada chunk recebido.
//
// Se o seq cria um gap (ex: recebe seq 5 mas seq 3 e 4 não chegaram),
// marca os seqs faltantes com timestamp de detecção.
func (gt *GapTracker) RecordChunk(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	gt.received[globalSeq] = true

	// Se este seq preenche um gap previamente detectado, remove das listas.
	delete(gt.firstSeen, globalSeq)
	delete(gt.notifiedGaps, globalSeq)

	now := time.Now()

	// Primeiro chunk da sessão: precisa tratar o caso em que seq 0 ainda não chegou.
	if !gt.hasSeenSeq {
		if globalSeq > 0 {
			for seq := uint32(0); seq < globalSeq; seq++ {
				if !gt.received[seq] {
					gt.firstSeen[seq] = now
				}
			}
		}
		gt.maxSeenSeq = globalSeq
		gt.hasSeenSeq = true
		return
	}

	// Atualiza maxSeenSeq e detecta novos gaps.
	if globalSeq > gt.maxSeenSeq {
		// Qualquer seq entre maxSeenSeq+1 e globalSeq-1 que não foi recebido é um gap potencial.
		for seq := gt.maxSeenSeq + 1; seq < globalSeq; seq++ {
			if !gt.received[seq] {
				if _, exists := gt.firstSeen[seq]; !exists {
					gt.firstSeen[seq] = now
				}
			}
		}
		gt.maxSeenSeq = globalSeq
	}
}

// CheckGaps retorna até maxNACKsPerCycle globalSeqs faltantes que persistem
// além do gapTimeout e ainda não foram notificados.
//
// Deve ser chamado periodicamente (ex: a cada 5s) pela goroutine de gap check.
// Retorna slice vazio se não há gaps persistentes ou todos já foram notificados.
// A marcação como "notificado" deve ocorrer apenas após envio bem-sucedido do NACK.
func (gt *GapTracker) CheckGaps() []uint32 {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	now := time.Now()
	var gaps []uint32
	keys := make([]uint32, 0, len(gt.firstSeen))

	for seq := range gt.firstSeen {
		keys = append(keys, seq)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, seq := range keys {
		detected := gt.firstSeen[seq]
		// Pula gaps já notificados (aguardando retransmissão).
		if gt.notifiedGaps[seq] {
			continue
		}
		// Pula gaps que chegaram enquanto isso.
		if gt.received[seq] {
			delete(gt.firstSeen, seq)
			delete(gt.notifiedGaps, seq)
			continue
		}
		// Pula gaps transientes (ainda dentro do timeout).
		if now.Sub(detected) < gt.gapTimeout {
			continue
		}
		gaps = append(gaps, seq)
		if len(gaps) >= gt.maxNACKsPerCycle {
			break
		}
	}

	return gaps
}

// MarkNotified marca um gap como tendo recebido NACK com sucesso.
// Deve ser chamado somente após o frame ControlNACK ter sido escrito sem erro.
func (gt *GapTracker) MarkNotified(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.received[globalSeq] {
		delete(gt.firstSeen, globalSeq)
		delete(gt.notifiedGaps, globalSeq)
		return
	}
	if _, exists := gt.firstSeen[globalSeq]; exists {
		gt.notifiedGaps[globalSeq] = true
	}
}

// RearmGap reinicia a janela de espera de um gap após o agent informar que
// retransmitiu o chunk. Se a retransmissão também se perder, um novo NACK poderá
// ser emitido após outro gapTimeout.
func (gt *GapTracker) RearmGap(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.received[globalSeq] {
		delete(gt.firstSeen, globalSeq)
		delete(gt.notifiedGaps, globalSeq)
		return
	}

	gt.firstSeen[globalSeq] = time.Now()
	delete(gt.notifiedGaps, globalSeq)
}

// ResolveGap marca um gap como resolvido. Deve ser usado apenas quando o
// servidor já recebeu de fato o chunk (ou equivalente), não apenas quando o
// agent confirmou a tentativa de retransmissão.
func (gt *GapTracker) ResolveGap(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	gt.received[globalSeq] = true
	delete(gt.firstSeen, globalSeq)
	delete(gt.notifiedGaps, globalSeq)
}

// PendingGaps retorna o número de gaps não resolvidos (detectados e ainda pendentes).
func (gt *GapTracker) PendingGaps() int {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	count := 0
	for seq := range gt.firstSeen {
		if !gt.received[seq] {
			count++
		}
	}
	return count
}

// MaxSeenSeq retorna o maior globalSeq visto até agora.
func (gt *GapTracker) MaxSeenSeq() uint32 {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return gt.maxSeenSeq
}
