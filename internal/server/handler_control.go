// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_control.go contém o processamento do Control Channel persistente
// e a lógica de Flow Rotation.
//
// O Control Channel é uma conexão TLS bidirecional de longa duração entre
// agent e server, usada para:
//   - Keep-alive (ControlPing/Pong) com medição de RTT
//   - Estatísticas do agent (CPU, memória, disco, load)
//   - AutoScaler stats (eficiência, streams ativos, modo)
//   - Progresso de backup (objetos escaneados/enviados)
//   - Sinalização de fim de ingestão (ControlIngestionDone)
//   - Orquestração de rotação de streams (ControlRotate/RotateACK)
//   - Gerenciamento de slots (ControlSlotPark/Resume)
//
// O Flow Rotation monitora o throughput por stream e solicita rotação
// dos que estão abaixo do threshold configurado, de forma graceful
// (via ControlRotate) ou abrupta (close direto).

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// rotateACKTimeout é o tempo máximo que o server espera pelo ControlRotateACK do agent.
// Se expirar, faz fallback para close abrupto (comportamento pré-Fase 3).
const rotateACKTimeout = 10 * time.Second

// handleControlChannel processa uma conexão de canal de controle persistente (CTRL).
// Usa despacho full-duplex baseado em magic bytes para suportar múltiplos tipos de frame:
// - ControlPing (CPNG): Agent → Server, responde com ControlPong
// - ControlRotateACK (CRAK): Agent → Server, sinaliza drain completo de stream
// O server também pode enviar frames assíncronos ao agent pela mesma conn:
// - ControlRotate (CROT): Server → Agent, solicita drain de stream
// O agent envia o keepalive_interval (uint32 big-endian, segundos) logo após o magic CTRL.
func (h *Handler) handleControlChannel(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	// Lê o keepalive_interval negociado pelo agent (4 bytes big-endian, segundos)
	intervalBuf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, intervalBuf); err != nil {
		logger.Error("control channel: reading keepalive interval", "error", err)
		return
	}
	conn.SetReadDeadline(time.Time{})

	intervalSecs := uint32(intervalBuf[0])<<24 | uint32(intervalBuf[1])<<16 | uint32(intervalBuf[2])<<8 | uint32(intervalBuf[3])
	if intervalSecs == 0 {
		intervalSecs = 30 // fallback default
	}

	// Read timeout = 2.5x keepalive_interval para tolerar jitter + 1 ping perdido
	readTimeout := time.Duration(intervalSecs) * time.Second * 5 / 2

	// Lê version do agent (string terminada em newline)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	clientVersion, err := readUntilNewline(conn)
	if err != nil {
		logger.Error("control channel: reading client version", "error", err)
		return
	}
	conn.SetReadDeadline(time.Time{})

	// Lê stats iniciais do agent (16B: CPU, Mem, Disk, Load)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	initialStats, err := protocol.ReadControlStatsPayload(conn)
	if err != nil {
		logger.Error("control channel: reading initial stats", "error", err)
		return
	}
	conn.SetReadDeadline(time.Time{})

	// Lê agent name do TLS peer cert CN (para registrar control conn por agent)
	agentName := h.extractAgentName(conn, logger)
	if agentName == "" {
		agentName = conn.RemoteAddr().String() // fallback
	}

	// Registra control conn e mutex de write para este agent
	writeMu := &sync.Mutex{}
	cci := &ControlConnInfo{
		Conn:          conn,
		ConnectedAt:   time.Now(),
		RemoteAddr:    conn.RemoteAddr().String(),
		KeepaliveS:    intervalSecs,
		ClientVersion: clientVersion,
	}
	cci.Stats.Store(&observability.AgentStats{
		CPUPercent:       initialStats.CPUPercent,
		MemoryPercent:    initialStats.MemoryPercent,
		DiskUsagePercent: initialStats.DiskUsagePercent,
		LoadAverage:      initialStats.LoadAverage,
	})
	h.registerControlConn(agentName, cci, writeMu)
	defer h.unregisterControlConn(agentName, cci, writeMu)

	// Sinaliza ControlLost para todas as sessões paralelas ativas deste agent
	// quando esta função retornar (control channel encerrado por qualquer motivo).
	defer func() {
		h.sessions.Range(func(_, value any) bool {
			ps, ok := value.(*ParallelSession)
			if !ok || ps.AgentName != agentName {
				return true
			}
			ps.signalControlLost()
			return true
		})
	}()

	logger = logger.With("agent", agentName)
	logger.Info("control channel established",
		"keepalive_interval_s", intervalSecs,
		"read_timeout", readTimeout,
	)

	// Emite evento de conexão do agente
	if h.Events != nil {
		h.Events.PushEvent("info", "agent_connected", agentName, fmt.Sprintf("control channel established (keepalive %ds, v%s)", intervalSecs, clientVersion), 0)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("control channel closing: server shutting down")
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))

		// Full-duplex: lê o magic de 4 bytes e despacha pelo tipo de frame
		magic, err := protocol.ReadControlMagic(conn)
		if err != nil {
			logger.Info("control channel closed", "reason", err)
			if h.Events != nil {
				h.Events.PushEvent("warn", "agent_disconnected", agentName, fmt.Sprintf("control channel closed: %v", err), 0)
			}
			return
		}

		switch magic {
		case protocol.MagicControlPing:
			// Agent enviou PING → responde com PONG
			ping, err := protocol.ReadControlPingPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading ping payload", "error", err)
				return
			}

			// Calcula server load simples (conexões ativas / 100)
			activeConns := float32(h.ActiveConns.Load())
			serverLoad := activeConns / 100.0
			if serverLoad > 1.0 {
				serverLoad = 1.0
			}

			// Disk free do volume atual (MB)
			var diskFree uint32
			var stat syscall.Statfs_t
			if err := syscall.Statfs(".", &stat); err == nil {
				// Bavail * Bsize = bytes available to non-root users
				// / 1024 / 1024 = MB
				diskFree = uint32(stat.Bavail * uint64(stat.Bsize) / 1024 / 1024)
			}

			writeMu.Lock()
			err = protocol.WriteControlPong(conn, ping, serverLoad, diskFree)
			writeMu.Unlock()
			if err != nil {
				logger.Warn("control channel pong write failed", "error", err)
				return
			}

			logger.Debug("control channel pong sent",
				"timestamp", ping,
				"server_load", serverLoad,
			)

		case protocol.MagicControlStats:
			// Agent enviou Stats
			stats, err := protocol.ReadControlStatsPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading stats payload", "error", err)
				return
			}

			// Atualiza stats na info da conexão
			if raw, ok := h.controlConns.Load(agentName); ok {
				cci := raw.(*ControlConnInfo)
				cci.Stats.Store(&observability.AgentStats{
					CPUPercent:       stats.CPUPercent,
					MemoryPercent:    stats.MemoryPercent,
					DiskUsagePercent: stats.DiskUsagePercent,
					LoadAverage:      stats.LoadAverage,
				})
			}

		case protocol.MagicControlAutoScaleStats:
			// Agent enviou AutoScale Stats
			asStats, err := protocol.ReadControlAutoScaleStatsPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading auto-scale stats payload", "error", err)
				return
			}

			// Mapeia state numérico para string
			stateStr := "stable"
			switch asStats.State {
			case protocol.AutoScaleStateScalingUp:
				stateStr = "scaling_up"
			case protocol.AutoScaleStateScaleDown:
				stateStr = "scaling_down"
			case protocol.AutoScaleStateProbing:
				stateStr = "probing"
			}

			info := &observability.AutoScaleInfo{
				Efficiency:    asStats.Efficiency,
				ProducerMBs:   asStats.ProducerMBs,
				DrainMBs:      asStats.DrainMBs,
				ActiveStreams: asStats.ActiveStreams,
				MaxStreams:    asStats.MaxStreams,
				State:         stateStr,
				ProbeActive:   asStats.ProbeActive == 1,
			}

			// Armazena na ParallelSession deste agent
			h.sessions.Range(func(_, value any) bool {
				ps, ok := value.(*ParallelSession)
				if !ok || ps.AgentName != agentName {
					return true
				}
				ps.AutoScaleInfo.Store(info)
				return false
			})

		case protocol.MagicControlRotateACK:
			// Agent confirmou drain de stream após ControlRotate
			streamIdx, err := protocol.ReadControlRotateACKPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading rotate ACK payload", "error", err)
				return
			}

			logger.Info("control channel: received ControlRotateACK", "stream", streamIdx)

			// Sinaliza o canal pendente em qualquer ParallelSession deste agent
			h.sessions.Range(func(_, value any) bool {
				ps, ok := value.(*ParallelSession)
				if !ok || ps.AgentName != agentName {
					return true
				}
				if int(streamIdx) < len(ps.Slots) {
					slot := ps.Slots[streamIdx]
					slot.RotatePendMu.Lock()
					ch := slot.RotatePend
					slot.RotatePendMu.Unlock()
					if ch != nil {
						select {
						case ch <- struct{}{}:
						default:
						}
						return false // encontrou e sinalizou, pode parar
					}
				}
				return true // esta sessão não tem RotatePend para este idx, continua
			})

		case protocol.MagicControlProgress:
			// Agent enviou progresso de backup (objetos escaneados/enviados)
			prog, err := protocol.ReadControlProgressPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading progress payload", "error", err)
				return
			}

			// Atualiza a ParallelSession deste agent
			h.sessions.Range(func(_, value any) bool {
				ps, ok := value.(*ParallelSession)
				if !ok || ps.AgentName != agentName {
					return true
				}
				ps.TotalObjects.Store(prog.TotalObjects)
				ps.ObjectsSent.Store(prog.ObjectsSent)
				if prog.WalkComplete {
					ps.WalkComplete.Store(1)
				}
				return false
			})

			logger.Debug("control channel: progress update",
				"total_objects", prog.TotalObjects,
				"objects_sent", prog.ObjectsSent,
				"walk_complete", prog.WalkComplete,
			)

		case protocol.MagicControlIngestionDone:
			// Agent sinalizou que toda a ingestão foi completada com sucesso
			cidnSessionID, err := protocol.ReadControlIngestionDonePayload(conn)
			if err != nil {
				logger.Warn("control channel: reading ControlIngestionDone payload", "error", err)
				return
			}

			logger.Info("control channel: received ControlIngestionDone", "session", cidnSessionID)

			// Lookup direto por sessionID — sem ambiguidade em multi-sessão
			if val, ok := h.sessions.Load(cidnSessionID); ok {
				if ps, ok := val.(*ParallelSession); ok {
					ps.ingestionOnce.Do(func() {
						close(ps.IngestionDone)
					})
				}
			} else {
				logger.Warn("control channel: ControlIngestionDone for unknown session", "session", cidnSessionID)
			}

			if h.Events != nil {
				h.Events.PushEvent("info", "ingestion_done_signal", agentName, fmt.Sprintf("agent confirmed all data sent (session %s)", cidnSessionID), 0)
			}

		case protocol.MagicControlSlotPark:
			// Agent desativou um slot (scale-down via auto-scaler)
			slotID, err := protocol.ReadControlSlotParkPayload(conn)
			if err != nil {
				logger.Warn("control channel: reading ControlSlotPark payload", "error", err)
				return
			}

			logger.Info("control channel: received ControlSlotPark", "slot", slotID)

			h.sessions.Range(func(_, value any) bool {
				ps, ok := value.(*ParallelSession)
				if !ok || ps.AgentName != agentName {
					return true
				}
				if int(slotID) < len(ps.Slots) {
					ps.Slots[slotID].SetStatus(SlotDisabled)
				}
				return false
			})

		case protocol.MagicControlSlotResume:
			// Agent reativou um slot (scale-up via auto-scaler)
			slotID, err := protocol.ReadControlSlotResumePayload(conn)
			if err != nil {
				logger.Warn("control channel: reading ControlSlotResume payload", "error", err)
				return
			}

			logger.Info("control channel: received ControlSlotResume", "slot", slotID)

			h.sessions.Range(func(_, value any) bool {
				ps, ok := value.(*ParallelSession)
				if !ok || ps.AgentName != agentName {
					return true
				}
				if int(slotID) < len(ps.Slots) {
					ps.Slots[slotID].SetStatus(SlotIdle)
				}
				return false
			})

		default:
			logger.Warn("control channel: unknown magic", "magic", string(magic[:]))
			return
		}
	}
}

// evaluateFlowRotation verifica se streams estão com throughput abaixo do threshold
// e solicita rotação graceful via canal de controle. Se o agent confirmar com
// ControlRotateACK, fecha a conexão de dados após drain. Se não houver canal de
// controle ou o ACK não chegar a tempo, faz fallback para close abrupto.
// Usa Swap(0) para ler os bytes reais do intervalo e resetar o contador.
// Limita a 1 rotação por tick para evitar tempestade de reconexões.
func (h *Handler) evaluateFlowRotation(intervalSecs float64) {
	const maxRotationsPerTick = 1

	frCfg := h.cfg.FlowRotation

	h.sessions.Range(func(key, value any) bool {
		ps, ok := value.(*ParallelSession)
		if !ok {
			return true
		}

		var rotated int

		for _, slot := range ps.Slots {
			// Swap-and-reset: lê bytes reais do intervalo e zera o contador.
			// Isso garante que o flow rotation veja o throughput real,
			// independente de quando logPerStreamStats roda.
			bytes := slot.TrafficIn.Swap(0)
			slot.TickBytes.Store(bytes)
			mbps := float64(bytes) / intervalSecs / (1024 * 1024)

			now := time.Now()

			// Sem tráfego no intervalo: stream pode estar apenas ocioso
			// (ex.: fim de pipeline ou producer momentaneamente sem dados).
			// Não tratar como degradação para evitar rotações desnecessárias.
			if bytes == 0 {
				slot.ClearSlowSince()
				continue
			}

			idx := slot.Index

			if mbps < frCfg.MinMBps {
				// Stream abaixo do threshold
				slowSince := slot.GetSlowSince()
				if slowSince.IsZero() {
					// Primeiro tick lento — marca início da degradação
					slot.SetSlowSince(now)
				} else {
					// Já estava marcado — verifica se passou eval_window
					sinceMarked := now.Sub(slowSince)

					// Verifica cooldown
					var sinceLast time.Duration
					lastReset := slot.GetLastReset()
					if lastReset.IsZero() {
						sinceLast = frCfg.Cooldown + 1 // nunca resetou, permite
					} else {
						sinceLast = now.Sub(lastReset)
					}

					if sinceMarked >= frCfg.EvalWindow && sinceLast >= frCfg.Cooldown && rotated < maxRotationsPerTick {
						h.rotateStream(key, ps, idx, mbps, sinceMarked)
						slot.SetLastReset(now)
						slot.ClearSlowSince()
						rotated++
					}
				}
			} else {
				// Stream acima do threshold — limpa marca
				slot.ClearSlowSince()
			}
		}

		return true
	})
}

// rotateStream executa a rotação de um stream, tentando primeiro a via graceful
// pelo canal de controle (ControlRotate → espera ACK → fecha conn). Se não houver
// canal de controle ou o ACK não chegar a tempo, faz fallback para close abrupto.
func (h *Handler) rotateStream(sessionKey any, ps *ParallelSession, idx uint8, mbps float64, slowFor time.Duration) {
	slot := ps.Slots[idx]
	slot.ConnMu.Lock()
	conn := slot.Conn
	slot.ConnMu.Unlock()
	if conn == nil {
		return
	}

	// Tenta graceful rotation via control channel
	ctrlInfo, hasCtrl := h.controlConns.Load(ps.AgentName)
	if hasCtrl {
		// Cria canal para esperar ACK do agent
		ackCh := make(chan struct{}, 1)
		slot.RotatePendMu.Lock()
		slot.RotatePend = ackCh
		slot.RotatePendMu.Unlock()
		defer func() {
			slot.RotatePendMu.Lock()
			slot.RotatePend = nil
			slot.RotatePendMu.Unlock()
		}()

		// Envia ControlRotate com mutex de write
		writeOK := false
		if muRaw, ok := h.controlConnsMu.Load(ps.AgentName); ok {
			mu := muRaw.(*sync.Mutex)
			mu.Lock()
			err := protocol.WriteControlRotate(ctrlInfo.(*ControlConnInfo).Conn, idx)
			mu.Unlock()
			writeOK = (err == nil)
			if err != nil {
				h.logger.Warn("flow rotation: failed to send ControlRotate, falling back",
					"agent", ps.AgentName, "stream", idx, "error", err)
			}
		}

		if writeOK {
			h.logger.Info("flow rotation: sent ControlRotate (graceful)",
				"session", sessionKey,
				"agent", ps.AgentName,
				"stream", idx,
				"mbps", fmt.Sprintf("%.2f", mbps),
				"slowFor", slowFor.String(),
			)

			// Emite evento de flow rotation graceful
			if h.Events != nil {
				h.Events.PushEvent("warn", "flow_rotation", ps.AgentName, fmt.Sprintf("stream %d rotated (graceful, %.2f MB/s, slow for %s)", idx, mbps, slowFor.String()), int(idx))
			}

			// Espera ACK com timeout
			select {
			case <-ackCh:
				h.logger.Info("flow rotation: received ControlRotateACK, closing data conn",
					"agent", ps.AgentName, "stream", idx)
				conn.Close()
				return
			case <-time.After(rotateACKTimeout):
				h.logger.Warn("flow rotation: ACK timeout, falling back to abrupt close",
					"agent", ps.AgentName, "stream", idx, "timeout", rotateACKTimeout)
			}
		}
	}

	// Fallback: close abrupto (comportamento pré-Fase 3)
	h.logger.Info("flow rotation triggered (abrupt)",
		"session", sessionKey,
		"agent", ps.AgentName,
		"stream", idx,
		"mbps", fmt.Sprintf("%.2f", mbps),
		"slowFor", slowFor.String(),
	)

	// Emite evento de flow rotation abrupta
	if h.Events != nil {
		h.Events.PushEvent("warn", "flow_rotation", ps.AgentName, fmt.Sprintf("stream %d rotated (abrupt, %.2f MB/s, slow for %s)", idx, mbps, slowFor.String()), int(idx))
	}

	conn.Close()
}
