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

type chunkPhase uint8

const (
	chunkPhaseInFlight chunkPhase = iota + 1
	chunkPhaseBuffered
)

type chunkState struct {
	phase        chunkPhase
	lastProgress time.Time
}

// GapTracker detecta chunks faltantes em sessões paralelas de forma proativa.
// Mantém um mapa de globalSeqs recebidos e detecta gaps persistentes quando
// um seq N+2 chega mas N+1 não — e esse gap persiste além do timeout configurado.
//
// Gaps transientes (chunks fora de ordem que chegam em poucos segundos) são tolerados.
// Apenas gaps que persistem além do timeout são reportados para envio de NACK.
type GapTracker struct {
	sessionID string

	// completed marca quais globalSeqs já foram efetivamente concluídos no assembler.
	completed map[uint32]bool

	// states rastreia chunks que já iniciaram leitura do payload, mas ainda não
	// foram concluídos no assembler. Isso permite ignorar chunks em voo ou já
	// bufferizados ao avaliar um gap.
	states map[uint32]chunkState

	// maxCompletedSeq é o maior globalSeq concluído no assembler até agora.
	maxCompletedSeq uint32

	// hasCompletedSeq evita ambiguidade com o valor zero de maxCompletedSeq.
	// Sem isso, um primeiro chunk fora de ordem (ex: seq 1) não detectaria o gap seq 0.
	hasCompletedSeq bool

	// firstSeen armazena quando cada gap foi detectado pela primeira vez.
	// Usado para tolerância de gaps transientes (out-of-order).
	firstSeen map[uint32]time.Time

	// notifiedGaps rastreia gaps já notificados via NACK, evitando duplicatas.
	notifiedGaps map[uint32]bool

	// gapTimeout é o tempo mínimo que um gap deve persistir antes de ser reportado.
	// Default: 2x streamReadDeadline (60s) para tolerar reconexões normais.
	gapTimeout time.Duration

	// inFlightTimeout é o tempo máximo sem progresso de payload para considerar
	// um chunk em voo como estagnado. Quando isso ocorre, ele volta a ser elegível
	// para retransmissão caso exista um gap para o seq correspondente.
	inFlightTimeout time.Duration

	// maxNACKsPerCycle limita quantos gaps são reportados por chamada de CheckGaps,
	// evitando flood de NACKs em degradação severa.
	maxNACKsPerCycle int

	mu     sync.Mutex
	logger *slog.Logger
}

// NewGapTracker cria um GapTracker para uma sessão paralela.
func NewGapTracker(sessionID string, gapTimeout, inFlightTimeout time.Duration, maxNACKsPerCycle int, logger *slog.Logger) *GapTracker {
	if maxNACKsPerCycle <= 0 {
		maxNACKsPerCycle = 5
	}
	if inFlightTimeout <= 0 {
		inFlightTimeout = 30 * time.Second
	}
	return &GapTracker{
		sessionID:        sessionID,
		completed:        make(map[uint32]bool),
		states:           make(map[uint32]chunkState),
		firstSeen:        make(map[uint32]time.Time),
		notifiedGaps:     make(map[uint32]bool),
		gapTimeout:       gapTimeout,
		inFlightTimeout:  inFlightTimeout,
		maxNACKsPerCycle: maxNACKsPerCycle,
		logger:           logger,
	}
}

// StartChunk marca o início da leitura do payload de um chunk.
// O chunk passa ao estado in-flight e ainda não é elegível como "completo".
func (gt *GapTracker) StartChunk(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.completed[globalSeq] {
		return
	}
	gt.states[globalSeq] = chunkState{
		phase:        chunkPhaseInFlight,
		lastProgress: time.Now(),
	}
}

// AdvanceChunk registra progresso real de leitura no payload de um chunk em voo.
func (gt *GapTracker) AdvanceChunk(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.completed[globalSeq] {
		return
	}
	state, ok := gt.states[globalSeq]
	if !ok {
		state = chunkState{phase: chunkPhaseInFlight}
	}
	state.lastProgress = time.Now()
	if state.phase == 0 {
		state.phase = chunkPhaseInFlight
	}
	gt.states[globalSeq] = state
}

// MarkBuffered marca que o chunk já foi lido por completo da rede e está
// preservado no chunk buffer, mas ainda não foi persistido pelo assembler.
func (gt *GapTracker) MarkBuffered(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.completed[globalSeq] {
		return
	}
	gt.states[globalSeq] = chunkState{
		phase:        chunkPhaseBuffered,
		lastProgress: time.Now(),
	}
}

// AbandonChunk remove o estado temporário de um chunk que falhou antes de ser
// aceito pelo assembler ou pelo chunk buffer.
func (gt *GapTracker) AbandonChunk(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if gt.completed[globalSeq] {
		return
	}
	delete(gt.states, globalSeq)
}

// CompleteChunk registra que o globalSeq foi concluído com sucesso pelo assembler.
// Apenas chunks completos avançam a janela de validação de gaps.
func (gt *GapTracker) CompleteChunk(globalSeq uint32) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	gt.completed[globalSeq] = true
	delete(gt.states, globalSeq)

	// Se este seq preenche um gap previamente detectado, remove das listas.
	delete(gt.firstSeen, globalSeq)
	delete(gt.notifiedGaps, globalSeq)

	now := time.Now()

	// Primeiro chunk da sessão: precisa tratar o caso em que seq 0 ainda não chegou.
	if !gt.hasCompletedSeq {
		if globalSeq > 0 {
			for seq := uint32(0); seq < globalSeq; seq++ {
				if !gt.completed[seq] {
					gt.firstSeen[seq] = now
				}
			}
		}
		gt.maxCompletedSeq = globalSeq
		gt.hasCompletedSeq = true
		return
	}

	// Atualiza maxCompletedSeq e detecta novos gaps.
	if globalSeq > gt.maxCompletedSeq {
		// Qualquer seq entre maxCompletedSeq+1 e globalSeq-1 que não foi concluído é um gap potencial.
		for seq := gt.maxCompletedSeq + 1; seq < globalSeq; seq++ {
			if !gt.completed[seq] {
				if _, exists := gt.firstSeen[seq]; !exists {
					gt.firstSeen[seq] = now
				}
			}
		}
		gt.maxCompletedSeq = globalSeq
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
		if gt.completed[seq] {
			delete(gt.firstSeen, seq)
			delete(gt.notifiedGaps, seq)
			delete(gt.states, seq)
			continue
		}
		// Pula chunks que já foram recebidos integralmente da rede e estão
		// aguardando apenas a drenagem do chunk buffer.
		if state, exists := gt.states[seq]; exists {
			switch state.phase {
			case chunkPhaseBuffered:
				continue
			case chunkPhaseInFlight:
				if now.Sub(state.lastProgress) < gt.inFlightTimeout {
					continue
				}
				// Chunk em voo estagnado: deixa o gap elegível imediatamente.
				gaps = append(gaps, seq)
				if len(gaps) >= gt.maxNACKsPerCycle {
					break
				}
				continue
			}
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

	if gt.completed[globalSeq] {
		delete(gt.firstSeen, globalSeq)
		delete(gt.notifiedGaps, globalSeq)
		delete(gt.states, globalSeq)
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

	if gt.completed[globalSeq] {
		delete(gt.firstSeen, globalSeq)
		delete(gt.notifiedGaps, globalSeq)
		delete(gt.states, globalSeq)
		return
	}

	gt.firstSeen[globalSeq] = time.Now()
	delete(gt.notifiedGaps, globalSeq)

	if state, exists := gt.states[globalSeq]; exists {
		state.lastProgress = time.Now()
		if state.phase == 0 {
			state.phase = chunkPhaseInFlight
		}
		gt.states[globalSeq] = state
	}
}

// ResolveGap marca um gap como resolvido. Deve ser usado apenas quando o
// servidor já recebeu de fato o chunk (ou equivalente), não apenas quando o
// agent confirmou a tentativa de retransmissão.
func (gt *GapTracker) ResolveGap(globalSeq uint32) {
	gt.CompleteChunk(globalSeq)
}

// PendingGaps retorna o número de gaps não resolvidos (detectados e ainda pendentes).
func (gt *GapTracker) PendingGaps() int {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	count := 0
	for seq := range gt.firstSeen {
		if !gt.completed[seq] {
			count++
		}
	}
	return count
}

// MaxSeenSeq retorna o maior globalSeq concluído até agora.
func (gt *GapTracker) MaxSeenSeq() uint32 {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return gt.maxCompletedSeq
}
