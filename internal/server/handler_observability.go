// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_observability.go contém todas as funções de métricas, snapshots
// e reporting que alimentam a WebUI e o endpoint Prometheus.
//
// Inclui:
//   - MetricsSnapshot / ChunkBufferStats — métricas globais do Handler
//   - ConnectedAgents — lista de agents conectados via control channel
//   - SessionsSnapshot / SessionDetail — visão de sessões ativas (list e detail)
//   - SessionHistorySnapshot / ActiveSessionHistorySnapshot — histórico
//   - StartStatsReporter / logPerStreamStats — logging periódico de métricas
//   - recordSessionEnd — registro de sessões finalizadas
//   - formatBytesGo / sessionStatus / streamStatus — funções utilitárias de UI

package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// MetricsSnapshot retorna uma cópia atômica das métricas observáveis.
// Implementa observability.HandlerMetrics.
func (h *Handler) MetricsSnapshot() observability.MetricsData {
	sessionCount := 0
	h.sessions.Range(func(_, _ interface{}) bool {
		sessionCount++
		return true
	})
	return observability.MetricsData{
		TrafficIn:   h.TrafficIn.Load(),
		DiskWrite:   h.DiskWrite.Load(),
		ActiveConns: h.ActiveConns.Load(),
		Sessions:    sessionCount,
		ChunkBuffer: h.ChunkBufferStats(),
	}
}

// ChunkBufferStats retorna as métricas do buffer de chunks em formato DTO.
// Retorna nil quando o buffer está desabilitado.
// Implementa observability.HandlerMetrics.
func (h *Handler) ChunkBufferStats() *observability.ChunkBufferDTO {
	if h.chunkBuffer == nil {
		return nil
	}
	s := h.chunkBuffer.Stats()
	return &observability.ChunkBufferDTO{
		Enabled:            s.Enabled,
		CapacityBytes:      s.CapacityBytes,
		InFlightBytes:      s.InFlightBytes,
		FillRatio:          s.FillRatio,
		TotalPushed:        s.TotalPushed,
		TotalDrained:       s.TotalDrained,
		TotalFallbacks:     s.TotalFallbacks,
		BackpressureEvents: s.BackpressureEvents,
		DrainRatio:         s.DrainRatio,
		DrainRateMBs:       s.DrainRateMBs,
	}
}

// ConnectedAgents retorna lista de agentes conectados via control channel.
// Implementa observability.HandlerMetrics.
func (h *Handler) ConnectedAgents() []observability.AgentInfo {
	var agents []observability.AgentInfo

	h.controlConns.Range(func(key, value interface{}) bool {
		agentName := key.(string)
		cci := value.(*ControlConnInfo)

		// Verifica se há sessão ativa para este agent e extrai client version
		hasSession := false
		clientVersion := cci.ClientVersion // versão do handshake do control channel
		h.sessions.Range(func(_, sv interface{}) bool {
			switch s := sv.(type) {
			case *PartialSession:
				if s.AgentName == agentName {
					hasSession = true
					if s.ClientVersion != "" {
						clientVersion = s.ClientVersion // sessão tem prioridade
					}
					return false
				}
			case *ParallelSession:
				if s.AgentName == agentName {
					hasSession = true
					if s.ClientVersion != "" {
						clientVersion = s.ClientVersion // sessão tem prioridade
					}
					return false
				}
			}
			return true
		})

		var stats *observability.AgentStats
		if s := cci.Stats.Load(); s != nil {
			stats = s.(*observability.AgentStats)
		}

		agents = append(agents, observability.AgentInfo{
			Name:          agentName,
			RemoteAddr:    cci.RemoteAddr,
			ConnectedAt:   cci.ConnectedAt.Format(time.RFC3339),
			ConnectedFor:  time.Since(cci.ConnectedAt).Truncate(time.Second).String(),
			KeepaliveS:    cci.KeepaliveS,
			HasSession:    hasSession,
			ClientVersion: clientVersion,
			Stats:         stats,
		})
		return true
	})

	return agents
}

// recordSessionEnd registra uma sessão finalizada no SessionHistoryRing.
// Chamado quando um backup (single ou parallel) termina com qualquer resultado.
func (h *Handler) recordSessionEnd(sessionID, agent, storage, backup, mode, compression, result string, startedAt time.Time, bytesTotal int64) {
	if h.SessionHistory == nil {
		return
	}
	now := time.Now()

	// Emite evento de sessão finalizada
	if h.Events != nil {
		level := "info"
		if result != "ok" {
			level = "error"
		}
		h.Events.PushEvent(level, "session_end", agent, fmt.Sprintf("%s/%s %s (%s)", storage, backup, result, mode), 0)
	}

	h.SessionHistory.Push(observability.SessionHistoryEntry{
		SessionID:   sessionID,
		Agent:       agent,
		Storage:     storage,
		Backup:      backup,
		Mode:        mode,
		Compression: compression,
		StartedAt:   startedAt.Format(time.RFC3339),
		FinishedAt:  now.Format(time.RFC3339),
		Duration:    now.Sub(startedAt).Truncate(time.Second).String(),
		BytesTotal:  bytesTotal,
		Result:      result,
	})
}

// SessionHistorySnapshot retorna as últimas sessões finalizadas.
func (h *Handler) SessionHistorySnapshot() []observability.SessionHistoryEntry {
	if h.SessionHistory == nil {
		return []observability.SessionHistoryEntry{}
	}
	return h.SessionHistory.Recent(0)
}

// ActiveSessionHistorySnapshot retorna snapshots periódicos de sessões ativas.
func (h *Handler) ActiveSessionHistorySnapshot(sessionID string, limit int) []observability.ActiveSessionSnapshotEntry {
	if h.ActiveSessionHistory == nil {
		return []observability.ActiveSessionSnapshotEntry{}
	}
	return h.ActiveSessionHistory.Recent(limit, sessionID)
}

// SessionsSnapshot retorna lista resumida de todas as sessões ativas.
// Implementa observability.HandlerMetrics.
func (h *Handler) SessionsSnapshot() []observability.SessionSummary {
	var sessions []observability.SessionSummary

	h.sessions.Range(func(key, value interface{}) bool {
		sessionID := key.(string)

		switch s := value.(type) {
		case *PartialSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			sessions = append(sessions, observability.SessionSummary{
				SessionID:     sessionID,
				Agent:         s.AgentName,
				Storage:       s.StorageName,
				Backup:        s.BackupName,
				Mode:          "single",
				Compression:   s.CompressionMode,
				StartedAt:     s.CreatedAt.Format(time.RFC3339),
				LastActivity:  lastAct.Format(time.RFC3339),
				BytesReceived: s.BytesWritten.Load(),
				ActiveStreams: 1,
				Status:        sessionStatus(lastAct),
			})

		case *ParallelSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			activeStreams := 0
			var totalOffset int64
			for _, slot := range s.Slots {
				if slot.GetStatus() == SlotReceiving {
					activeStreams++
				}
				totalOffset += slot.Offset.Load()
			}
			// Progresso e ETA
			totalObj := s.TotalObjects.Load()
			sentObj := s.ObjectsSent.Load()
			walkDone := s.WalkComplete.Load() != 0
			eta := "∞"
			if walkDone && totalObj > 0 && sentObj > 0 && sentObj < totalObj {
				elapsed := time.Since(s.CreatedAt).Seconds()
				rate := float64(sentObj) / elapsed
				remaining := float64(totalObj - sentObj)
				etaSecs := remaining / rate
				eta = (time.Duration(etaSecs) * time.Second).Truncate(time.Second).String()
			} else if walkDone && sentObj >= totalObj && totalObj > 0 {
				eta = "0s"
			}

			// Assembly stats (single call)
			asmStats := s.Assembler.Stats()

			// AssemblyETA: calculado quando o assembler está na fase assembling
			var assemblyETA string
			if asmStats.Phase == "assembling" && asmStats.TotalChunks > 0 && asmStats.AssembledChunks > 0 && !asmStats.AssemblyStartedAt.IsZero() {
				elapsed := time.Since(asmStats.AssemblyStartedAt).Seconds()
				rate := float64(asmStats.AssembledChunks) / elapsed
				remaining := float64(asmStats.TotalChunks - asmStats.AssembledChunks)
				if rate > 0 {
					etaSecs := remaining / rate
					assemblyETA = (time.Duration(etaSecs) * time.Second).Truncate(time.Second).String()
				}
			}

			// Status: se Closing=true, a transferência acabou e o assembler está finalizando
			status := sessionStatus(lastAct)
			if s.Closing.Load() {
				status = "finalizing"
			}

			summary := observability.SessionSummary{
				SessionID:      sessionID,
				Agent:          s.AgentName,
				Storage:        s.StorageName,
				Backup:         s.BackupName,
				Mode:           "parallel",
				Compression:    s.StorageInfo.CompressionMode,
				StartedAt:      s.CreatedAt.Format(time.RFC3339),
				LastActivity:   lastAct.Format(time.RFC3339),
				BytesReceived:  totalOffset,
				DiskWriteBytes: s.DiskWriteBytes.Load(),
				ActiveStreams:  activeStreams,
				MaxStreams:     int(s.MaxStreams),
				Status:         status,
				TotalObjects:   totalObj,
				ObjectsSent:    sentObj,
				WalkComplete:   walkDone,
				ETA:            eta,
				AssemblyETA:    assemblyETA,
				Assembler: &observability.AssemblerStats{
					NextExpectedSeq: asmStats.NextExpectedSeq,
					PendingChunks:   asmStats.PendingChunks,
					PendingMemBytes: asmStats.PendingMemBytes,
					TotalBytes:      asmStats.TotalBytes,
					Finalized:       asmStats.Finalized,
					TotalChunks:     asmStats.TotalChunks,
					AssembledChunks: asmStats.AssembledChunks,
					Phase:           asmStats.Phase,
				},
			}
			// Dados de buffer por sessão (zero quando buffer desabilitado).
			if h.chunkBuffer != nil {
				bufBytes := h.chunkBuffer.SessionBytes(s.Assembler)
				summary.BufferEnabled = true
				summary.BufferInFlightBytes = bufBytes
				if h.chunkBuffer.Stats().CapacityBytes > 0 {
					summary.BufferFillPercent = float64(bufBytes) / float64(h.chunkBuffer.Stats().CapacityBytes) * 100
				}
			}
			sessions = append(sessions, summary)

			// Auto-scale info (presente apenas se o agent enviou stats)
			if raw := s.AutoScaleInfo.Load(); raw != nil {
				sessions[len(sessions)-1].AutoScale = raw.(*observability.AutoScaleInfo)
			}
		}
		return true
	})

	return sessions
}

// SessionDetail retorna detalhe de uma sessão incluindo streams.
// Implementa observability.HandlerMetrics.
func (h *Handler) SessionDetail(id string) (*observability.SessionDetail, bool) {
	raw, ok := h.sessions.Load(id)
	if !ok {
		return nil, false
	}

	switch s := raw.(type) {
	case *PartialSession:
		lastAct := time.Unix(0, s.LastActivity.Load())
		return &observability.SessionDetail{
			SessionSummary: observability.SessionSummary{
				SessionID:     id,
				Agent:         s.AgentName,
				Storage:       s.StorageName,
				Backup:        s.BackupName,
				Mode:          "single",
				Compression:   s.CompressionMode,
				StartedAt:     s.CreatedAt.Format(time.RFC3339),
				LastActivity:  lastAct.Format(time.RFC3339),
				BytesReceived: s.BytesWritten.Load(),
				ActiveStreams: 1,
				Status:        sessionStatus(lastAct),
			},
		}, true

	case *ParallelSession:
		lastAct := time.Unix(0, s.LastActivity.Load())
		activeStreams := 0
		var totalOffset int64
		var streams []observability.StreamDetail

		for _, slot := range s.Slots {
			status := slot.GetStatus()
			active := status == SlotReceiving
			if active {
				activeStreams++
			}

			offset := slot.Offset.Load()
			totalOffset += offset

			// Throughput por stream: usa TickBytes (snapshot do último
			// tick de 15s) quando FlowRotation está habilitado, pois o
			// evaluateFlowRotation faz Swap(0) nos contadores TrafficIn.
			// Sem FlowRotation, lê direto do TrafficIn acumulado.
			var mbps float64
			if h.cfg.FlowRotation.Enabled {
				mbps = float64(slot.TickBytes.Load()) / 15.0 / (1024 * 1024)
			} else {
				mbps = float64(slot.TrafficIn.Load()) / 15.0 / (1024 * 1024)
			}

			// Idle time
			var idleSecs int64
			lastNano := slot.LastActivity.Load()
			if lastNano > 0 {
				idleSecs = int64(time.Since(time.Unix(0, lastNano)).Seconds())
			}

			// Slow since
			var slowSince string
			if ss := slot.GetSlowSince(); !ss.IsZero() {
				slowSince = ss.Format(time.RFC3339)
			}

			// Stream uptime e reconnects
			var connectedFor string
			if ct := slot.GetConnectedAt(); !ct.IsZero() {
				connectedFor = time.Since(ct).Truncate(time.Second).String()
			}
			reconnects := slot.Reconnects.Load()

			streams = append(streams, observability.StreamDetail{
				Index:               slot.Index,
				OffsetBytes:         offset,
				MBps:                mbps,
				IdleSecs:            idleSecs,
				SlowSince:           slowSince,
				Active:              active,
				Status:              streamStatus(active, idleSecs, slowSince, h.cfg.FlowRotation.EvalWindow, status),
				ConnectedFor:        connectedFor,
				Reconnects:          reconnects,
				Rotations:           slot.Rotations.Load(),
				ChunksReceived:      slot.ChunksReceived.Load(),
				ChunksLost:          slot.ChunksLost.Load(),
				ChunksRetransmitted: slot.ChunksRetransmitted.Load(),
				LastChunkSeq:        slot.LastChunkSeq.Load(),
			})
		}

		// Ordena streams por index para exibição determinística na UI.
		sort.Slice(streams, func(i, j int) bool {
			return streams[i].Index < streams[j].Index
		})

		// Progresso e ETA
		totalObj := s.TotalObjects.Load()
		sentObj := s.ObjectsSent.Load()
		walkDone := s.WalkComplete.Load() != 0
		eta := "∞"
		if walkDone && totalObj > 0 && sentObj > 0 && sentObj < totalObj {
			elapsed := time.Since(s.CreatedAt).Seconds()
			rate := float64(sentObj) / elapsed // objetos/s
			remaining := float64(totalObj - sentObj)
			etaSecs := remaining / rate
			eta = (time.Duration(etaSecs) * time.Second).Truncate(time.Second).String()
		} else if walkDone && sentObj >= totalObj && totalObj > 0 {
			eta = "0s"
		}

		// Assembly stats (single call)
		asmStats := s.Assembler.Stats()

		// AssemblyETA: calculado quando o assembler está na fase assembling
		var assemblyETA string
		if asmStats.Phase == "assembling" && asmStats.TotalChunks > 0 && asmStats.AssembledChunks > 0 && !asmStats.AssemblyStartedAt.IsZero() {
			elapsed := time.Since(asmStats.AssemblyStartedAt).Seconds()
			rate := float64(asmStats.AssembledChunks) / elapsed
			remaining := float64(asmStats.TotalChunks - asmStats.AssembledChunks)
			if rate > 0 {
				etaSecs := remaining / rate
				assemblyETA = (time.Duration(etaSecs) * time.Second).Truncate(time.Second).String()
			}
		}

		// Status: se Closing=true, a transferência acabou e o assembler está finalizando
		detailStatus := sessionStatus(lastAct)
		if s.Closing.Load() {
			detailStatus = "finalizing"
		}

		detail := &observability.SessionDetail{
			SessionSummary: observability.SessionSummary{
				SessionID:      id,
				Agent:          s.AgentName,
				Storage:        s.StorageName,
				Backup:         s.BackupName,
				Mode:           "parallel",
				Compression:    s.StorageInfo.CompressionMode,
				StartedAt:      s.CreatedAt.Format(time.RFC3339),
				LastActivity:   lastAct.Format(time.RFC3339),
				BytesReceived:  totalOffset,
				DiskWriteBytes: s.DiskWriteBytes.Load(),
				ActiveStreams:  activeStreams,
				MaxStreams:     int(s.MaxStreams),
				Status:         detailStatus,
				TotalObjects:   totalObj,
				ObjectsSent:    sentObj,
				WalkComplete:   walkDone,
				ETA:            eta,
				AssemblyETA:    assemblyETA,
				Assembler: &observability.AssemblerStats{
					NextExpectedSeq: asmStats.NextExpectedSeq,
					PendingChunks:   asmStats.PendingChunks,
					PendingMemBytes: asmStats.PendingMemBytes,
					TotalBytes:      asmStats.TotalBytes,
					Finalized:       asmStats.Finalized,
					TotalChunks:     asmStats.TotalChunks,
					AssembledChunks: asmStats.AssembledChunks,
					Phase:           asmStats.Phase,
				},
			},
			Streams: streams,
		}

		// Auto-scale info
		if raw := s.AutoScaleInfo.Load(); raw != nil {
			detail.AutoScale = raw.(*observability.AutoScaleInfo)
		}

		// Dados de buffer por sessão (zero quando buffer desabilitado).
		if h.chunkBuffer != nil {
			bufBytes := h.chunkBuffer.SessionBytes(s.Assembler)
			detail.BufferEnabled = true
			detail.BufferInFlightBytes = bufBytes
			if cap := h.chunkBuffer.Stats().CapacityBytes; cap > 0 {
				detail.BufferFillPercent = float64(bufBytes) / float64(cap) * 100
			}
		}

		return detail, true
	}

	return nil, false
}

// formatBytesGo formata bytes em string legível (ex: "12.3 MB").
func formatBytesGo(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// sessionStatus determina o status de uma sessão baseado na última atividade.
func sessionStatus(lastActivity time.Time) string {
	idle := time.Since(lastActivity)
	switch {
	case idle < 10*time.Second:
		return "running"
	case idle < 60*time.Second:
		return "idle"
	default:
		return "degraded"
	}
}

// streamStatus determina o status textual de um stream individual.
// Considera o estado do slot (disabled, disconnected), throughput (slow/degraded)
// e tempo de inatividade (idle).
func streamStatus(active bool, idleSecs int64, slowSince string, evalWindow time.Duration, slotStatus SlotStatus) string {
	if !active {
		if slotStatus == SlotDisabled {
			return "disabled"
		}
		return "disconnected"
	}
	if slowSince != "" {
		if t, err := time.Parse(time.RFC3339, slowSince); err == nil {
			if time.Since(t) >= evalWindow {
				return "degraded"
			}
		}
		return "slow"
	}
	switch {
	case idleSecs < 10:
		return "running"
	case idleSecs < 60:
		return "idle"
	default:
		return "degraded"
	}
}

// streamStat representa stats de um único stream para log estruturado.
type streamStat struct {
	Idx     uint8  `json:"idx"`
	MBps    string `json:"MBps"`
	IdleSec int64  `json:"idle_s"`
}

// StartStatsReporter imprime métricas do server a cada 15 segundos:
// conexões ativas, traffic in (MB/s), disk write (MB/s), sessões abertas.
// Quando logging.stream_stats=true, imprime também stats por stream.
func (h *Handler) StartStatsReporter(ctx context.Context) {
	const interval = 15 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			secs := interval.Seconds()

			// Flow Rotation ANTES do reset de counters globais.
			// evaluateFlowRotation faz Swap(0) nos StreamTrafficIn,
			// garantindo que lê os bytes reais do intervalo.
			if h.cfg.FlowRotation.Enabled {
				h.evaluateFlowRotation(secs)
			}

			// Swap-and-reset: lê o acumulado e zera
			trafficIn := h.TrafficIn.Swap(0)
			diskWrite := h.DiskWrite.Swap(0)
			conns := h.ActiveConns.Load()

			// Conta sessões abertas (parciais + paralelas)
			var sessionCount int
			h.sessions.Range(func(_, _ interface{}) bool {
				sessionCount++
				return true
			})

			// Calcula taxas em MB/s
			trafficMBps := float64(trafficIn) / secs / (1024 * 1024)
			diskMBps := float64(diskWrite) / secs / (1024 * 1024)

			h.logger.Info("server stats",
				"conns", conns,
				"sessions", sessionCount,
				"traffic_in_MBps", fmt.Sprintf("%.2f", trafficMBps),
				"disk_write_MBps", fmt.Sprintf("%.2f", diskMBps),
				"traffic_in_total_MB", fmt.Sprintf("%.1f", float64(trafficIn)/(1024*1024)),
				"disk_write_total_MB", fmt.Sprintf("%.1f", float64(diskWrite)/(1024*1024)),
			)

			// Per-stream stats (configurável) — usa Load() porque
			// evaluateFlowRotation já fez Swap(0) nos counters.
			if h.cfg.Logging.StreamStats {
				h.logPerStreamStats(secs)
			}
		}
	}
}

// logPerStreamStats itera sessões paralelas e loga stats por stream.
func (h *Handler) logPerStreamStats(intervalSecs float64) {
	h.sessions.Range(func(key, value any) bool {
		ps, ok := value.(*ParallelSession)
		if !ok {
			return true // pula PartialSession
		}

		var stats []streamStat
		for _, slot := range ps.Slots {
			// Se flow rotation está habilitado, os contadores já foram
			// resetados por evaluateFlowRotation (Swap(0)) neste tick.
			// Caso contrário, precisamos fazer o Swap(0) aqui para
			// exibir a taxa do intervalo (não acumulativa).
			var bytes int64
			if h.cfg.FlowRotation.Enabled {
				// Em flow rotation, o contador já foi consumido por evaluateFlowRotation.
				// Usa snapshot do último tick para manter o log fiel ao intervalo.
				bytes = slot.TickBytes.Load()
			} else {
				bytes = slot.TrafficIn.Swap(0)
			}
			mbps := float64(bytes) / intervalSecs / (1024 * 1024)

			var idleSec int64
			lastNano := slot.LastActivity.Load()
			if lastNano > 0 {
				idleSec = int64(time.Since(time.Unix(0, lastNano)).Seconds())
			}

			stats = append(stats, streamStat{
				Idx:     slot.Index,
				MBps:    fmt.Sprintf("%.2f", mbps),
				IdleSec: idleSec,
			})
		}

		if len(stats) > 0 {
			h.logger.Info("stream stats",
				"session", key,
				"agent", ps.AgentName,
				"streams", len(stats),
				"detail", stats,
			)
		}
		return true
	})
}
