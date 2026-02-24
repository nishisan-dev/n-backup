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

// avgSlotSize é o tamanho médio assumido de um chunk para calcular a
// capacidade do canal. Não impacta o tamanho real dos dados armazenados —
// serve apenas para dimensionar o número de slots no canal buffered.
const avgSlotSize = 1 * 1024 * 1024 // 1MB

// chunkBufferPushTimeout é o tempo máximo que Push aguarda por um slot livre
// antes de retornar backpressure ao chamador.
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
	Capacity           int
	InFlight           int
	TotalPushed        int64
	TotalDrained       int64
	BackpressureEvents int64
	DrainMode          string
}

// ChunkBuffer é um buffer de chunks em memória global e compartilhado entre
// sessões paralelas. Absorve chunks recebidos da rede e os drena assincronamente
// para o assembler de destino, suavizando picos de escrita em disco.
//
// Quando nil ou desabilitado (SizeRaw == 0), o caminho de escrita padrão
// é usado sem nenhuma alteração de comportamento.
type ChunkBuffer struct {
	slots     chan chunkSlot
	drainMode string
	logger    *slog.Logger

	// Métricas atômicas
	totalPushed        atomic.Int64
	totalDrained       atomic.Int64
	backpressureEvents atomic.Int64
}

// NewChunkBuffer cria um ChunkBuffer com base na configuração.
// Retorna nil quando o buffer está desabilitado (SizeRaw == 0).
func NewChunkBuffer(cfg config.ChunkBufferConfig, logger *slog.Logger) *ChunkBuffer {
	if cfg.SizeRaw <= 0 {
		return nil
	}

	// Calcula o número de slots: tamanho total / tamanho médio por chunk.
	// Mínimo de 2 slots para evitar deadlock imediato.
	capacity := int(cfg.SizeRaw / avgSlotSize)
	if capacity < 2 {
		capacity = 2
	}

	drainMode := cfg.DrainMode
	if drainMode == "" {
		drainMode = "lazy"
	}

	logger.Info("chunk buffer initialized",
		"capacity_slots", capacity,
		"size_mb", cfg.SizeRaw/(1024*1024),
		"drain_mode", drainMode,
	)

	return &ChunkBuffer{
		slots:     make(chan chunkSlot, capacity),
		drainMode: drainMode,
		logger:    logger,
	}
}

// Enabled retorna true quando o buffer está ativo.
func (cb *ChunkBuffer) Enabled() bool {
	return cb != nil
}

// Push envia um chunk ao buffer. Bloqueia até chunkBufferPushTimeout se o
// buffer estiver cheio, depois retorna erro de backpressure.
// Deve ser chamado, portanto sempre sem o mutex do assembler held.
func (cb *ChunkBuffer) Push(globalSeq uint32, data []byte, assembler *ChunkAssembler) error {
	// Copia os dados para garantir que o buffer seja o único dono do slice —
	// o caller pode reutilizar a memória original após Push retornar.
	buf := make([]byte, len(data))
	copy(buf, data)

	slot := chunkSlot{
		globalSeq: globalSeq,
		data:      buf,
		assembler: assembler,
	}

	select {
	case cb.slots <- slot:
		cb.totalPushed.Add(1)
		return nil
	case <-time.After(chunkBufferPushTimeout):
		cb.backpressureEvents.Add(1)
		return fmt.Errorf("chunk buffer full after %s (backpressure): seq %d dropped",
			chunkBufferPushTimeout, globalSeq)
	}
}

// StartDrainer inicia a goroutine de drenagem. Deve ser chamado uma única vez,
// logo após a criação do buffer. Para quando ctx é cancelado.
func (cb *ChunkBuffer) StartDrainer(ctx context.Context) {
	go cb.drainLoop(ctx)
}

// drainLoop lê slots do canal e os entrega ao assembler de cada sessão.
func (cb *ChunkBuffer) drainLoop(ctx context.Context) {
	cb.logger.Info("chunk buffer drainer started", "drain_mode", cb.drainMode)
	for {
		select {
		case <-ctx.Done():
			// Drena slots restantes antes de encerrar para não perder dados
			// de sessões em andamento no momento do shutdown graceful.
			cb.drainRemaining()
			cb.logger.Info("chunk buffer drainer stopped")
			return
		case slot := <-cb.slots:
			cb.drainSlot(slot)
		}
	}
}

// drainRemaining consome o canal até ele estar vazio (shutdown graceful).
func (cb *ChunkBuffer) drainRemaining() {
	for {
		select {
		case slot := <-cb.slots:
			cb.drainSlot(slot)
		default:
			return
		}
	}
}

// drainSlot entrega um único slot ao assembler de destino.
func (cb *ChunkBuffer) drainSlot(slot chunkSlot) {
	r := bytes.NewReader(slot.data)
	if err := slot.assembler.WriteChunk(slot.globalSeq, r, int64(len(slot.data))); err != nil {
		cb.logger.Error("chunk buffer drain error",
			"globalSeq", slot.globalSeq,
			"error", err,
		)
		// Não propaga o erro — o assembler já loga internamente.
		// O chunk perdido será detectado no Finalize como seq ausente.
	}
	cb.totalDrained.Add(1)
}

// Stats retorna um snapshot das métricas do buffer.
func (cb *ChunkBuffer) Stats() ChunkBufferStats {
	if cb == nil {
		return ChunkBufferStats{Enabled: false}
	}
	return ChunkBufferStats{
		Enabled:            true,
		Capacity:           cap(cb.slots),
		InFlight:           len(cb.slots),
		TotalPushed:        cb.totalPushed.Load(),
		TotalDrained:       cb.totalDrained.Load(),
		BackpressureEvents: cb.backpressureEvents.Load(),
		DrainMode:          cb.drainMode,
	}
}
