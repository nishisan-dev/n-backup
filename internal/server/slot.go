// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// SlotStatus representa o estado de um slot no protocolo v5.
type SlotStatus uint8

const (
	// SlotIdle indica que o slot foi pré-alocado mas nunca conectou.
	SlotIdle SlotStatus = iota
	// SlotReceiving indica que o slot está ativamente recebendo chunks.
	SlotReceiving
	// SlotDisconnected indica que o slot perdeu a conexão (pode reconectar).
	SlotDisconnected
	// SlotDisabled indica que o slot foi desativado pelo agent (scale-down via ControlSlotPark).
	SlotDisabled
)

// String retorna a representação textual do SlotStatus para exibição na WebUI/API.
func (s SlotStatus) String() string {
	switch s {
	case SlotIdle:
		return "idle"
	case SlotReceiving:
		return "receiving"
	case SlotDisconnected:
		return "disconnected"
	case SlotDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// Slot representa um slot individual dentro de uma sessão paralela.
// Unifica os 12 sync.Map da ParallelSession anterior em uma struct tipada.
// Cada slot é pré-alocado no ParallelInitACK e acessado por index (sem sync.Map).
//
// Padrão de concorrência:
//   - Campos atomic (offset, trafficIn, lastActivity, etc.) podem ser lidos/escritos
//     concorrentemente sem lock.
//   - ConnMu protege Conn (substituição durante re-join).
//   - CancelFn é escrito apenas por handleParallelJoin (serializado por stream index
//     no protocolo — o agent não envia dois joins para o mesmo slot simultaneamente).
type Slot struct {
	Index uint8 // índice do slot (0..MaxStreams-1), imutável

	// --- Conexão ---
	Conn   net.Conn   // conexão corrente deste slot (nil se nunca conectou ou desconectado)
	ConnMu sync.Mutex // protege Conn durante re-join

	// --- Status ---
	Status atomic.Uint32 // SlotStatus (cast para uint32 para atomic)

	// --- Offsets e tráfego ---
	Offset    atomic.Int64 // bytes recebidos no total (para resume no re-join)
	TrafficIn atomic.Int64 // bytes recebidos no intervalo corrente (para stats)
	TickBytes atomic.Int64 // snapshot do tráfego do último tick (para flow rotation)

	// --- Atividade ---
	LastActivity atomic.Int64 // UnixNano do último I/O bem-sucedido

	// --- Cancelamento ---
	CancelFn context.CancelFunc // cancela goroutine anterior em re-join

	// --- Flow rotation ---
	SlowSince    atomic.Value  // time.Time — início de degradação contínua (zero value = não degradado)
	LastReset    atomic.Value  // time.Time — último flow rotation deste slot
	RotatePend   chan struct{} // sinalizada por ControlRotateACK; nil = sem rotação pendente
	RotatePendMu sync.Mutex    // protege RotatePend

	// --- Uptime ---
	ConnectedAt atomic.Value // time.Time — timestamp do último connect/re-join
	Reconnects  atomic.Int32 // reconexões acumuladas

	// --- Chunk metrics (v5) ---
	ChunksReceived      atomic.Uint32 // total de chunks recebidos por este slot
	ChunksLost          atomic.Uint32 // chunks reportados como perdidos
	ChunksRetransmitted atomic.Uint32 // chunks retransmitidos para este slot
	LastChunkSeq        atomic.Uint32 // GlobalSeq do último chunk recebido
}

// NewSlot cria um Slot pré-alocado com estado inicial Idle.
func NewSlot(index uint8) *Slot {
	s := &Slot{
		Index: index,
	}
	s.Status.Store(uint32(SlotIdle))
	return s
}

// GetStatus retorna o SlotStatus corrente.
func (s *Slot) GetStatus() SlotStatus {
	return SlotStatus(s.Status.Load())
}

// SetStatus atualiza o SlotStatus atomicamente.
func (s *Slot) SetStatus(status SlotStatus) {
	s.Status.Store(uint32(status))
}

// GetSlowSince retorna o time.Time de início de degradação, ou zero value se não degradado.
func (s *Slot) GetSlowSince() time.Time {
	v := s.SlowSince.Load()
	if v == nil {
		return time.Time{}
	}
	return v.(time.Time)
}

// SetSlowSince define o início da degradação.
func (s *Slot) SetSlowSince(t time.Time) {
	s.SlowSince.Store(t)
}

// ClearSlowSince limpa a marca de degradação.
func (s *Slot) ClearSlowSince() {
	s.SlowSince.Store(time.Time{})
}

// GetLastReset retorna o time.Time do último flow rotation.
func (s *Slot) GetLastReset() time.Time {
	v := s.LastReset.Load()
	if v == nil {
		return time.Time{}
	}
	return v.(time.Time)
}

// SetLastReset define o timestamp do último flow rotation.
func (s *Slot) SetLastReset(t time.Time) {
	s.LastReset.Store(t)
}

// GetConnectedAt retorna o time.Time da última conexão.
func (s *Slot) GetConnectedAt() time.Time {
	v := s.ConnectedAt.Load()
	if v == nil {
		return time.Time{}
	}
	return v.(time.Time)
}

// SetConnectedAt define o timestamp da conexão.
func (s *Slot) SetConnectedAt(t time.Time) {
	s.ConnectedAt.Store(t)
}

// PreallocateSlots cria MaxStreams slots pré-alocados com estado Idle.
func PreallocateSlots(maxStreams uint8) []*Slot {
	slots := make([]*Slot, maxStreams)
	for i := uint8(0); i < maxStreams; i++ {
		slots[i] = NewSlot(i)
	}
	return slots
}
