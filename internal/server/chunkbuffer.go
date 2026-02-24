// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

// avgSlotSize é o tamanho médio assumido de um chunk para dimensionar o canal
// de slots em número de entradas (não impacta o limite real de bytes).
const avgSlotSize = 1 * 1024 * 1024 // 1MB

// drainPollInterval é o intervalo de polling do drainer quando drain_ratio > 0.
const drainPollInterval = 5 * time.Millisecond

// chunkBufferFlushTimeout é o tempo máximo que Flush aguarda o buffer da sessão esvaziar.
const chunkBufferFlushTimeout = 30 * time.Second

// chunkBufferPushTimeout é o tempo máximo que Push aguarda por espaço livre no
// canal de slots antes de retornar backpressure ao chamador.
const chunkBufferPushTimeout = 5 * time.Second

// chunkSlot representa um chunk em trânsito no buffer de memória.
type chunkSlot struct {
	globalSeq uint32
	data      []byte
	assembler *ChunkAssembler
}

// ChunkBufferStats contém métricas instantâneas do buffer de chunks.
type ChunkBufferStats struct {
	Enabled            bool
	CapacityBytes      int64
	InFlightBytes      int64
	FillRatio          float64
	TotalPushed        int64
	TotalDrained       int64
	TotalFallbacks     int64
	BackpressureEvents int64
	DrainRatio         float64
}

// ChunkBuffer é um buffer de chunks em memória global e compartilhado entre
// sessões paralelas. Funciona como um filesystem virtual em memória:
// cada chunk é escrito por inteiro antes de ser considerado "aceito".
//
// Comportamento por drain_ratio (configurado via DrainRatioRaw):
//   - 0.0: write-through — assim que o chunk é fechado no buffer, inicia o drain
//   - 0.0–1.0: drena quando inFlightBytes/sizeRaw >= drain_ratio
//
// Fallback: se o chunk for maior que o espaço disponível, é entregue diretamente
// ao assembler (que persiste conforme seu assembler_mode: lazy=staging, eager=append).
//
// Flush() é scoped por sessão (por *ChunkAssembler): aguarda apenas os chunks
// daquela sessão serem drenados, sem bloquear sessões concorrentes.
type ChunkBuffer struct {
	sizeRaw    int64   // capacidade total em bytes (imutável)
	drainRatio float64 // limiar de fill para acionar drain (0.0 a 1.0)
	logger     *slog.Logger

	slots       chan chunkSlot // canal com slots de chunks pendentes
	drainSignal chan struct{}  // sinal para drain imediato

	// inFlightBytes rastreia bytes reservados (global, para Stats e fallback check).
	// FIX #2: modificado via CAS para evitar race na reserva de capacidade.
	inFlightBytes atomic.Int64

	// sessionBytes rastreia bytes em voo por sessão (*ChunkAssembler → *atomic.Int64).
	// FIX #1: permite Flush() aguardar apenas os bytes da sessão solicitante.
	sessionBytes sync.Map

	// Métricas atômicas
	totalPushed        atomic.Int64
	totalDrained       atomic.Int64
	totalFallbacks     atomic.Int64
	backpressureEvents atomic.Int64
}

// NewChunkBuffer cria um ChunkBuffer com base na configuração.
// Retorna nil quando o buffer está desabilitado (SizeRaw == 0).
func NewChunkBuffer(cfg config.ChunkBufferConfig, logger *slog.Logger) *ChunkBuffer {
	if cfg.SizeRaw <= 0 {
		return nil
	}

	capacity := int(cfg.SizeRaw / avgSlotSize)
	if capacity < 2 {
		capacity = 2
	}

	drainRatio := cfg.DrainRatioRaw

	logger.Info("chunk buffer initialized",
		"capacity_bytes_mb", cfg.SizeRaw/(1024*1024),
		"channel_slots", capacity,
		"drain_ratio", drainRatio,
	)

	return &ChunkBuffer{
		sizeRaw:     cfg.SizeRaw,
		drainRatio:  drainRatio,
		logger:      logger,
		slots:       make(chan chunkSlot, capacity),
		drainSignal: make(chan struct{}, 1),
	}
}

// Enabled retorna true quando o buffer está ativo.
func (cb *ChunkBuffer) Enabled() bool {
	return cb != nil
}

// getSessionCounter retorna (criando se necessário) o contador atômico de bytes
// em voo para uma sessão específica, identificada pelo ponteiro do assembler.
func (cb *ChunkBuffer) getSessionCounter(a *ChunkAssembler) *atomic.Int64 {
	v, _ := cb.sessionBytes.LoadOrStore(a, &atomic.Int64{})
	return v.(*atomic.Int64)
}

// Push tenta inserir um chunk no buffer em memória.
//
// FIX #2 — Race condition: a reserva de bytes usa um loop CAS para garantir
// atomicidade entre a verificação de capacidade e o incremento de inFlightBytes.
//
// Se o chunk não couber, usa fallback direto ao assembler.
// Se drain_ratio == 0, sinaliza drenagem imediata após inserção.
func (cb *ChunkBuffer) Push(globalSeq uint32, data []byte, assembler *ChunkAssembler) error {
	dataLen := int64(len(data))

	// FIX #2: reserva de bytes via CAS — evita race entre goroutines concorrentes.
	// Loop até conseguir reservar atomicamente ou determinar que não há espaço.
	for {
		current := cb.inFlightBytes.Load()
		available := cb.sizeRaw - current
		if dataLen > available {
			// Fallback: chunk não cabe — entrega diretamente ao assembler.
			cb.logger.Debug("chunk buffer fallback: chunk exceeds available capacity",
				"globalSeq", globalSeq,
				"chunkBytes", dataLen,
				"availableBytes", available,
			)
			cb.totalFallbacks.Add(1)
			return assembler.WriteChunk(globalSeq, bytes.NewReader(data), dataLen)
		}
		if cb.inFlightBytes.CompareAndSwap(current, current+dataLen) {
			// Reserva atômica bem-sucedida.
			break
		}
		// CAS falhou: outro goroutine modificou inFlightBytes — tenta novamente.
	}

	// Incrementa contador da sessão (para Flush scoped — FIX #1).
	cb.getSessionCounter(assembler).Add(dataLen)

	slot := chunkSlot{
		globalSeq: globalSeq,
		data:      data,
		assembler: assembler,
	}

	// Tenta enviar ao canal com timeout de backpressure.
	select {
	case cb.slots <- slot:
		cb.totalPushed.Add(1)
	case <-time.After(chunkBufferPushTimeout):
		// Backpressure: desfaz reserva e retorna erro.
		cb.inFlightBytes.Add(-dataLen)
		cb.getSessionCounter(assembler).Add(-dataLen)
		cb.backpressureEvents.Add(1)
		return fmt.Errorf("chunk buffer full after %s (backpressure): seq %d",
			chunkBufferPushTimeout, globalSeq)
	}

	// Aciona o drainer conforme o drain_ratio.
	if cb.drainRatio == 0 {
		cb.signalDrain()
	} else {
		fillRatio := float64(cb.inFlightBytes.Load()) / float64(cb.sizeRaw)
		if fillRatio >= cb.drainRatio {
			cb.signalDrain()
		}
	}

	return nil
}

// signalDrain envia um sinal não-bloqueante ao drainer.
func (cb *ChunkBuffer) signalDrain() {
	select {
	case cb.drainSignal <- struct{}{}:
	default:
	}
}

// StartDrainer inicia a goroutine de drenagem. Deve ser chamada uma única vez.
func (cb *ChunkBuffer) StartDrainer(ctx context.Context) {
	go cb.drainLoop(ctx)
}

// drainLoop gerencia o ciclo de vida do drainer.
func (cb *ChunkBuffer) drainLoop(ctx context.Context) {
	cb.logger.Info("chunk buffer drainer started", "drain_ratio", cb.drainRatio)

	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cb.drainAll()
			cb.logger.Info("chunk buffer drainer stopped")
			return

		case <-cb.drainSignal:
			cb.drainAll()

		case <-ticker.C:
			if cb.drainRatio > 0 {
				fillRatio := float64(cb.inFlightBytes.Load()) / float64(cb.sizeRaw)
				if fillRatio >= cb.drainRatio {
					cb.drainAll()
				}
			}
		}
	}
}

// drainAll consome todos os slots disponíveis no momento no canal.
func (cb *ChunkBuffer) drainAll() {
	for {
		select {
		case slot := <-cb.slots:
			cb.drainSlot(slot)
		default:
			return
		}
	}
}

// drainSlot entrega um único slot ao assembler de destino e libera os bytes reservados.
func (cb *ChunkBuffer) drainSlot(slot chunkSlot) {
	dataLen := int64(len(slot.data))

	if err := slot.assembler.WriteChunk(slot.globalSeq, bytes.NewReader(slot.data), dataLen); err != nil {
		cb.logger.Error("chunk buffer drain error",
			"globalSeq", slot.globalSeq,
			"error", err,
		)
	}

	// Libera bytes globais e por sessão — após WriteChunk para que
	// Flush() só retorne com a escrita concluída.
	cb.inFlightBytes.Add(-dataLen)
	cb.getSessionCounter(slot.assembler).Add(-dataLen)
	cb.totalDrained.Add(1)
}

// Flush aguarda que todos os chunks da sessão identificada por assembler sejam
// drenados completamente para o assembler.
//
// FIX #1 — Flush scoped por sessão: não aguarda bytes de outras sessões,
// eliminando o risco de bloquear sessões concorrentes (ex: sessão A não espera
// pela sessão B que ainda está recebendo chunks).
//
// Deve ser chamado antes de assembler.Finalize() para garantir integridade.
func (cb *ChunkBuffer) Flush(assembler *ChunkAssembler) error {
	if cb == nil {
		return nil
	}

	counter := cb.getSessionCounter(assembler)
	if counter.Load() == 0 {
		return nil
	}

	// Força drain imediato independente do ratio.
	cb.signalDrain()

	deadline := time.Now().Add(chunkBufferFlushTimeout)
	for time.Now().Before(deadline) {
		if counter.Load() == 0 {
			cb.sessionBytes.Delete(assembler)
			return nil
		}
		cb.signalDrain()
		time.Sleep(drainPollInterval)
	}

	remaining := counter.Load()
	return fmt.Errorf("chunk buffer flush timeout after %s: %d bytes still in flight for session",
		chunkBufferFlushTimeout, remaining)
}

// Stats retorna um snapshot das métricas globais do buffer.
func (cb *ChunkBuffer) Stats() ChunkBufferStats {
	if cb == nil {
		return ChunkBufferStats{Enabled: false}
	}
	inFlight := cb.inFlightBytes.Load()
	var fillRatio float64
	if cb.sizeRaw > 0 {
		fillRatio = float64(inFlight) / float64(cb.sizeRaw)
	}
	return ChunkBufferStats{
		Enabled:            true,
		CapacityBytes:      cb.sizeRaw,
		InFlightBytes:      inFlight,
		FillRatio:          fillRatio,
		TotalPushed:        cb.totalPushed.Load(),
		TotalDrained:       cb.totalDrained.Load(),
		TotalFallbacks:     cb.totalFallbacks.Load(),
		BackpressureEvents: cb.backpressureEvents.Load(),
		DrainRatio:         cb.drainRatio,
	}
}
