// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

// avgSlotSize é o tamanho médio assumido de um chunk para dimensionar o canal
// de slots em número de entradas (não impacta o limite real de bytes).
const avgSlotSize = 1 * 1024 * 1024 // 1MB

// drainPollInterval é o intervalo de polling do drainer quando drain_ratio > 0.
// O drainer acorda a cada intervalo para verificar se o limiar foi atingido.
const drainPollInterval = 5 * time.Millisecond

// chunkBufferFlushTimeout é o tempo máximo que Flush aguarda o buffer esvaziar.
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
// Comportamento por drain_ratio:
//   - 0.0: write-through — assim que o chunk é fechado no buffer, inicia o drain
//   - 0.0–1.0: drena quando inFlightBytes/sizeRaw >= drain_ratio
//   - No fallback (chunk maior que capacidade disponível): chama WriteChunk
//     diretamente no assembler, que decide conforme seu assembler_mode
//     (lazy → staging em disco; eager → append no arquivo final).
//
// O drain_mode foi removido: é o assembler_mode do storage (lazy|eager) que
// determina como os chunks são escritos em disco, não o buffer.
type ChunkBuffer struct {
	sizeRaw    int64   // capacidade total em bytes (imutável)
	drainRatio float64 // limiar de fill para acionar drain (0.0 a 1.0)
	logger     *slog.Logger

	slots       chan chunkSlot // canal com slots de chunks pendentes
	drainSignal chan struct{}  // sinal para drain imediato (ratio=0 ou threshold atingido)

	// Rastreamento de bytes em voo (para ratio e fallback)
	inFlightBytes atomic.Int64

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

	// Capacidade em número de slots: sizeRaw / avgSlotSize, mínimo 2.
	// Apenas limita quantos chunks simultâneos podem estar no canal;
	// o controle real de memória é feito em bytes via inFlightBytes.
	capacity := int(cfg.SizeRaw / avgSlotSize)
	if capacity < 2 {
		capacity = 2
	}

	drainRatio := cfg.DrainRatio

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

// Push tenta inserir um chunk no buffer em memória.
//
// Se o chunk não couber no espaço disponível (inFlightBytes + len(data) > sizeRaw),
// o caminho de fallback é usado: o chunk é entregue diretamente ao assembler,
// que persiste conforme seu assembler_mode (lazy = staging, eager = append final).
//
// Se drain_ratio == 0, sinaliza drenagem imediata após inserção.
// Se drain_ratio > 0, verifica o limiar e sinaliza se atingido.
func (cb *ChunkBuffer) Push(globalSeq uint32, data []byte, assembler *ChunkAssembler) error {
	dataLen := int64(len(data))

	// Verifica se o chunk cabe no espaço disponível do buffer.
	current := cb.inFlightBytes.Load()
	available := cb.sizeRaw - current
	if dataLen > available {
		// Fallback: o chunk excede a capacidade disponível.
		// Entrega diretamente ao assembler (respeita seu assembler_mode).
		cb.logger.Debug("chunk buffer fallback: chunk exceeds available capacity",
			"globalSeq", globalSeq,
			"chunkBytes", dataLen,
			"availableBytes", available,
			"inFlightBytes", current,
		)
		cb.totalFallbacks.Add(1)
		return assembler.WriteChunk(globalSeq, bytes.NewReader(data), dataLen)
	}

	// Reserva espaço em bytes antes de tentar enviar ao canal.
	cb.inFlightBytes.Add(dataLen)

	slot := chunkSlot{
		globalSeq: globalSeq,
		data:      data, // ownership transferido — o caller não reutiliza o slice
		assembler: assembler,
	}

	// Tenta enviar ao canal com timeout de backpressure.
	select {
	case cb.slots <- slot:
		cb.totalPushed.Add(1)
	case <-time.After(chunkBufferPushTimeout):
		// Backpressure: canal cheio — desfaz reserva e retorna erro.
		cb.inFlightBytes.Add(-dataLen)
		cb.backpressureEvents.Add(1)
		return fmt.Errorf("chunk buffer full after %s (backpressure): seq %d",
			chunkBufferPushTimeout, globalSeq)
	}

	// Aciona o drainer conforme o drain_ratio.
	if cb.drainRatio == 0 {
		// Write-through: drain imediato após cada chunk aceito.
		cb.signalDrain()
	} else {
		// Drain baseado em limiar de fill.
		fillRatio := float64(cb.inFlightBytes.Load()) / float64(cb.sizeRaw)
		if fillRatio >= cb.drainRatio {
			cb.signalDrain()
		}
	}

	return nil
}

// signalDrain envia um sinal não-bloqueante ao drainer para acionar drenagem.
func (cb *ChunkBuffer) signalDrain() {
	select {
	case cb.drainSignal <- struct{}{}:
	default:
		// Já há um sinal pendente — drainer será acionado em breve.
	}
}

// StartDrainer inicia a goroutine de drenagem. Deve ser chamada uma única vez.
// Para quando ctx é cancelado, após drenar os slots restantes (graceful).
func (cb *ChunkBuffer) StartDrainer(ctx context.Context) {
	go cb.drainLoop(ctx)
}

// drainLoop gerencia o ciclo de vida do drainer.
// Para drain_ratio=0: reage ao drainSignal para cada chunk aceito.
// Para drain_ratio>0: reage ao drainSignal (threshold) e a um ticker de poll.
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

// drainSlot entrega um único slot ao assembler de destino e contabiliza os bytes.
func (cb *ChunkBuffer) drainSlot(slot chunkSlot) {
	dataLen := int64(len(slot.data))

	r := bytes.NewReader(slot.data)
	if err := slot.assembler.WriteChunk(slot.globalSeq, r, dataLen); err != nil {
		cb.logger.Error("chunk buffer drain error",
			"globalSeq", slot.globalSeq,
			"error", err,
		)
		// O assembler já loga internamente; aqui apenas decrementamos.
	}

	// Libera bytes reservados — deve ocorrer após WriteChunk para que o
	// Flush() só retorne após a escrita estar concluída.
	cb.inFlightBytes.Add(-dataLen)
	cb.totalDrained.Add(1)
}

// Flush aguarda o buffer esvaziar completamente (inFlightBytes == 0).
// Deve ser chamado antes de assembler.Finalize() para garantir que todos os
// chunks do buffer tenham sido entregues ao assembler.
// Sinaliza o drainer para forçar drain imediato e aguarda até timeout.
func (cb *ChunkBuffer) Flush() error {
	if cb == nil || cb.inFlightBytes.Load() == 0 {
		return nil
	}

	// Força drain imediato independente do ratio.
	cb.signalDrain()

	deadline := time.Now().Add(chunkBufferFlushTimeout)
	for time.Now().Before(deadline) {
		if cb.inFlightBytes.Load() == 0 {
			return nil
		}
		// Continua sinalizando para o drainer não perder o sinal
		cb.signalDrain()
		time.Sleep(drainPollInterval)
	}

	remaining := cb.inFlightBytes.Load()
	return fmt.Errorf("chunk buffer flush timeout after %s: %d bytes still in flight",
		chunkBufferFlushTimeout, remaining)
}

// Stats retorna um snapshot das métricas do buffer.
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
