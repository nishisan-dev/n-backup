// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/protocol"
	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// sackInterval define a cada quantos bytes o server envia um SACK.
// Reduzido para 1MB para evitar deadlock com buffers pequenos no agent.
const sackInterval = 1 * 1024 * 1024 // 1MB

// readInactivityTimeout é o tempo máximo de inatividade na leitura de dados (single-stream).
// Se expirar, a conexão é considerada morta e a goroutine é liberada.
const readInactivityTimeout = 90 * time.Second

// streamReadDeadline é o deadline de read para streams paralelos.
// Menor que readInactivityTimeout porque streams paralelos têm reconexão automática:
// quanto mais rápido detectar a falha, mais rápido o agent pode reconectar.
const streamReadDeadline = 30 * time.Second

// sackWriteTimeout é o deadline de write para envio de SACKs/ChunkSACKs.
const sackWriteTimeout = 10 * time.Second

// PartialSession rastreia um backup parcial para resume.
type PartialSession struct {
	TmpPath       string
	BytesWritten  int64
	AgentName     string
	StorageName   string
	BackupName    string
	BaseDir       string
	CreatedAt     time.Time
	LastActivity  atomic.Int64 // UnixNano do último I/O bem-sucedido
	ClientVersion string       // Versão do client (protocolo v3+)
}

// Handler processa conexões individuais de backup.
type Handler struct {
	cfg      *config.ServerConfig
	logger   *slog.Logger
	locks    *sync.Map // Mapa de locks por "agent:storage"
	sessions *sync.Map // Mapa de sessões parciais por sessionID

	// Control channel registry: agentName → *ControlConnInfo
	// Registrado em handleControlChannel, usado por evaluateFlowRotation
	// para enviar ControlRotate graceful, e por ConnectedAgents para observabilidade.
	controlConns   sync.Map // agentName (string) → *ControlConnInfo
	controlConnsMu sync.Map // agentName (string) → *sync.Mutex (protege writes na conn)

	// Métricas observáveis pelo stats reporter
	TrafficIn   atomic.Int64 // bytes recebidos da rede (acumulado desde último reset)
	DiskWrite   atomic.Int64 // bytes escritos em disco (acumulado desde último reset)
	ActiveConns atomic.Int32 // conexões ativas no momento

	// Events ring buffer para observabilidade (nil quando WebUI desabilitada).
	Events *observability.EventRing
}

// ControlConnInfo armazena metadata de um control channel conectado.
type ControlConnInfo struct {
	Conn        net.Conn
	ConnectedAt time.Time
	RemoteAddr  string
	KeepaliveS  uint32
	Stats       atomic.Value // *observability.AgentStats
}

// NewHandler cria um novo Handler.
func NewHandler(cfg *config.ServerConfig, logger *slog.Logger, locks *sync.Map, sessions *sync.Map) *Handler {
	return &Handler{
		cfg:      cfg,
		logger:   logger,
		locks:    locks,
		sessions: sessions,
	}
}

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
		clientVersion := ""
		h.sessions.Range(func(_, sv interface{}) bool {
			switch s := sv.(type) {
			case *PartialSession:
				if s.AgentName == agentName {
					hasSession = true
					clientVersion = s.ClientVersion
					return false
				}
			case *ParallelSession:
				if s.AgentName == agentName {
					hasSession = true
					clientVersion = s.ClientVersion
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

// SessionsSnapshot retorna lista de sessões ativas como DTOs.
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
				StartedAt:     s.CreatedAt.Format(time.RFC3339),
				LastActivity:  lastAct.Format(time.RFC3339),
				BytesReceived: s.BytesWritten,
				ActiveStreams: 1,
				Status:        sessionStatus(lastAct),
			})

		case *ParallelSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			activeStreams := 0
			var totalOffset int64
			s.StreamConns.Range(func(_, _ interface{}) bool {
				activeStreams++
				return true
			})
			s.StreamOffsets.Range(func(_, v interface{}) bool {
				if ptr, ok := v.(*int64); ok {
					totalOffset += atomic.LoadInt64(ptr)
				}
				return true
			})
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

			sessions = append(sessions, observability.SessionSummary{
				SessionID:      sessionID,
				Agent:          s.AgentName,
				Storage:        s.StorageName,
				Backup:         s.BackupName,
				Mode:           "parallel",
				StartedAt:      s.CreatedAt.Format(time.RFC3339),
				LastActivity:   lastAct.Format(time.RFC3339),
				BytesReceived:  totalOffset,
				DiskWriteBytes: s.DiskWriteBytes.Load(),
				ActiveStreams:  activeStreams,
				MaxStreams:     int(s.MaxStreams),
				Status:         sessionStatus(lastAct),
				TotalObjects:   totalObj,
				ObjectsSent:    sentObj,
				WalkComplete:   walkDone,
				ETA:            eta,
				Assembler: &observability.AssemblerStats{
					NextExpectedSeq: s.Assembler.Stats().NextExpectedSeq,
					PendingChunks:   s.Assembler.Stats().PendingChunks,
					PendingMemBytes: s.Assembler.Stats().PendingMemBytes,
					TotalBytes:      s.Assembler.Stats().TotalBytes,
					Finalized:       s.Assembler.Stats().Finalized,
				},
			})
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
				StartedAt:     s.CreatedAt.Format(time.RFC3339),
				LastActivity:  lastAct.Format(time.RFC3339),
				BytesReceived: s.BytesWritten,
				ActiveStreams: 1,
				Status:        sessionStatus(lastAct),
			},
		}, true

	case *ParallelSession:
		lastAct := time.Unix(0, s.LastActivity.Load())
		activeStreams := 0
		var totalOffset int64
		var streams []observability.StreamDetail
		activeByIndex := make(map[uint8]bool)

		s.StreamConns.Range(func(k, _ interface{}) bool {
			if idx, ok := k.(uint8); ok {
				activeByIndex[idx] = true
			}
			activeStreams++
			return true
		})

		s.StreamOffsets.Range(func(k, v interface{}) bool {
			idx := k.(uint8)
			var offset int64
			if ptr, ok := v.(*int64); ok {
				offset = atomic.LoadInt64(ptr)
			}
			totalOffset += offset

			// Throughput por stream: usa StreamTickBytes (snapshot do último
			// tick de 15s) quando FlowRotation está habilitado, pois o
			// evaluateFlowRotation faz Swap(0) nos contadores StreamTrafficIn.
			// Sem FlowRotation, lê direto do StreamTrafficIn acumulado.
			var mbps float64
			if h.cfg.FlowRotation.Enabled {
				if tickRaw, ok := s.StreamTickBytes.Load(idx); ok {
					mbps = float64(tickRaw.(int64)) / 15.0 / (1024 * 1024)
				}
			} else if trafficRaw, ok := s.StreamTrafficIn.Load(idx); ok {
				if ai, ok := trafficRaw.(*atomic.Int64); ok {
					bytes := ai.Load()
					mbps = float64(bytes) / 15.0 / (1024 * 1024)
				}
			}

			// Idle time
			var idleSecs int64
			if lastActRaw, ok := s.StreamLastAct.Load(idx); ok {
				if ai, ok := lastActRaw.(*atomic.Int64); ok {
					lastIO := time.Unix(0, ai.Load())
					idleSecs = int64(time.Since(lastIO).Seconds())
				}
			}

			// Slow since
			var slowSince string
			if ssRaw, ok := s.StreamSlowSince.Load(idx); ok {
				if ts, ok := ssRaw.(time.Time); ok && !ts.IsZero() {
					slowSince = ts.Format(time.RFC3339)
				}
			}
			active := activeByIndex[idx]

			// Stream uptime e reconnects
			var connectedFor string
			if ct, ok := s.StreamConnectedAt.Load(idx); ok {
				connectedFor = time.Since(ct.(time.Time)).Truncate(time.Second).String()
			}
			var reconnects int32
			if rc, ok := s.StreamReconnects.Load(idx); ok {
				reconnects = rc.(*atomic.Int32).Load()
			}

			streams = append(streams, observability.StreamDetail{
				Index:        idx,
				OffsetBytes:  offset,
				MBps:         mbps,
				IdleSecs:     idleSecs,
				SlowSince:    slowSince,
				Active:       active,
				Status:       streamStatus(active, idleSecs, slowSince, h.cfg.FlowRotation.EvalWindow),
				ConnectedFor: connectedFor,
				Reconnects:   reconnects,
			})
			return true
		})

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

		return &observability.SessionDetail{
			SessionSummary: observability.SessionSummary{
				SessionID:      id,
				Agent:          s.AgentName,
				Storage:        s.StorageName,
				Backup:         s.BackupName,
				Mode:           "parallel",
				StartedAt:      s.CreatedAt.Format(time.RFC3339),
				LastActivity:   lastAct.Format(time.RFC3339),
				BytesReceived:  totalOffset,
				DiskWriteBytes: s.DiskWriteBytes.Load(),
				ActiveStreams:  activeStreams,
				MaxStreams:     int(s.MaxStreams),
				Status:         sessionStatus(lastAct),
				TotalObjects:   totalObj,
				ObjectsSent:    sentObj,
				WalkComplete:   walkDone,
				ETA:            eta,
				Assembler: &observability.AssemblerStats{
					NextExpectedSeq: s.Assembler.Stats().NextExpectedSeq,
					PendingChunks:   s.Assembler.Stats().PendingChunks,
					PendingMemBytes: s.Assembler.Stats().PendingMemBytes,
					TotalBytes:      s.Assembler.Stats().TotalBytes,
					Finalized:       s.Assembler.Stats().Finalized,
				},
			},
			Streams: streams,
		}, true
	}

	return nil, false
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

func streamStatus(active bool, idleSecs int64, slowSince string, evalWindow time.Duration) string {
	if !active {
		return "inactive"
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

// streamStat representa stats de um único stream para log estruturado.
type streamStat struct {
	Idx     uint8  `json:"idx"`
	MBps    string `json:"MBps"`
	IdleSec int64  `json:"idle_s"`
}

// logPerStreamStats itera sessões paralelas e loga stats por stream.
func (h *Handler) logPerStreamStats(intervalSecs float64) {
	h.sessions.Range(func(key, value any) bool {
		ps, ok := value.(*ParallelSession)
		if !ok {
			return true // pula PartialSession
		}

		var stats []streamStat
		ps.StreamTrafficIn.Range(func(k, v any) bool {
			idx := k.(uint8)
			counter := v.(*atomic.Int64)
			// Se flow rotation está habilitado, os contadores já foram
			// resetados por evaluateFlowRotation (Swap(0)) neste tick.
			// Caso contrário, precisamos fazer o Swap(0) aqui para
			// exibir a taxa do intervalo (não acumulativa).
			var bytes int64
			if h.cfg.FlowRotation.Enabled {
				// Em flow rotation, o contador já foi consumido por evaluateFlowRotation.
				// Usa snapshot do último tick para manter o log fiel ao intervalo.
				if tickBytes, ok := ps.StreamTickBytes.Load(idx); ok {
					bytes = tickBytes.(int64)
				} else {
					bytes = 0
				}
			} else {
				bytes = counter.Swap(0)
			}
			mbps := float64(bytes) / intervalSecs / (1024 * 1024)

			var idleSec int64
			if lastAct, ok := ps.StreamLastAct.Load(idx); ok {
				lastNano := lastAct.(*atomic.Int64).Load()
				if lastNano > 0 {
					idleSec = int64(time.Since(time.Unix(0, lastNano)).Seconds())
				}
			}

			stats = append(stats, streamStat{
				Idx:     idx,
				MBps:    fmt.Sprintf("%.2f", mbps),
				IdleSec: idleSec,
			})
			return true
		})

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

// rotateACKTimeout é o tempo máximo que o server espera pelo ControlRotateACK do agent.
// Se expirar, faz fallback para close abrupto (comportamento pré-Fase 3).
const rotateACKTimeout = 10 * time.Second

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

		ps.StreamTrafficIn.Range(func(k, v any) bool {
			idx := k.(uint8)
			counter := v.(*atomic.Int64)

			// Swap-and-reset: lê bytes reais do intervalo e zera o contador.
			// Isso garante que o flow rotation veja o throughput real,
			// independente de quando logPerStreamStats roda.
			bytes := counter.Swap(0)
			ps.StreamTickBytes.Store(idx, bytes)
			mbps := float64(bytes) / intervalSecs / (1024 * 1024)

			now := time.Now()

			// Sem tráfego no intervalo: stream pode estar apenas ocioso
			// (ex.: fim de pipeline ou producer momentaneamente sem dados).
			// Não tratar como degradação para evitar rotações desnecessárias.
			if bytes == 0 {
				ps.StreamSlowSince.Delete(idx)
				return true
			}

			if mbps < frCfg.MinMBps {
				// Stream abaixo do threshold
				if _, loaded := ps.StreamSlowSince.LoadOrStore(idx, now); loaded {
					// Já estava marcado — verifica se passou eval_window
					slowSince, _ := ps.StreamSlowSince.Load(idx)
					sinceMarked := now.Sub(slowSince.(time.Time))

					// Verifica cooldown
					var sinceLast time.Duration
					if lastReset, ok := ps.StreamLastReset.Load(idx); ok {
						sinceLast = now.Sub(lastReset.(time.Time))
					} else {
						sinceLast = frCfg.Cooldown + 1 // nunca resetou, permite
					}

					if sinceMarked >= frCfg.EvalWindow && sinceLast >= frCfg.Cooldown && rotated < maxRotationsPerTick {
						h.rotateStream(key, ps, idx, mbps, sinceMarked)
						ps.StreamLastReset.Store(idx, now)
						ps.StreamSlowSince.Delete(idx)
						rotated++
					}
				}
			} else {
				// Stream acima do threshold — limpa marca
				ps.StreamSlowSince.Delete(idx)
			}

			return true
		})

		return true
	})
}

// rotateStream executa a rotação de um stream, tentando primeiro a via graceful
// pelo canal de controle (ControlRotate → espera ACK → fecha conn). Se não houver
// canal de controle ou o ACK não chegar a tempo, faz fallback para close abrupto.
func (h *Handler) rotateStream(sessionKey any, ps *ParallelSession, idx uint8, mbps float64, slowFor time.Duration) {
	conn, ok := ps.StreamConns.Load(idx)
	if !ok {
		return
	}

	// Tenta graceful rotation via control channel
	ctrlInfo, hasCtrl := h.controlConns.Load(ps.AgentName)
	if hasCtrl {
		// Cria canal para esperar ACK do agent
		ackCh := make(chan struct{}, 1)
		ps.RotatePending.Store(idx, ackCh)
		defer ps.RotatePending.Delete(idx)

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

			// Espera ACK com timeout
			select {
			case <-ackCh:
				h.logger.Info("flow rotation: received ControlRotateACK, closing data conn",
					"agent", ps.AgentName, "stream", idx)
				conn.(net.Conn).Close()
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
	conn.(net.Conn).Close()
}

// HandleConnection processa uma conexão individual de backup.
func (h *Handler) HandleConnection(ctx context.Context, conn net.Conn) {
	h.ActiveConns.Add(1)
	defer h.ActiveConns.Add(-1)
	defer conn.Close()

	logger := h.logger.With("remote", conn.RemoteAddr().String())

	// Lê os primeiros 4 bytes para determinar o tipo de sessão
	magic := make([]byte, 4)
	if _, err := io.ReadFull(conn, magic); err != nil {
		logger.Error("reading magic bytes", "error", err)
		return
	}

	switch string(magic) {
	case "PING":
		h.handleHealthCheck(conn, logger)
	case "NBKP":
		h.handleBackup(ctx, conn, logger)
	case "RSME":
		h.handleResume(ctx, conn, logger)
	case "PJIN":
		h.handleParallelJoin(ctx, conn, logger)
	case "CTRL":
		h.handleControlChannel(ctx, conn, logger)
	default:
		logger.Warn("unknown magic bytes", "magic", string(magic))
	}
}

// handleHealthCheck processa um health check PING.
func (h *Handler) handleHealthCheck(conn net.Conn, logger *slog.Logger) {
	logger.Debug("health check received")

	// TODO: implementar disk free real com syscall.Statfs
	diskFree := uint64(0)

	if err := protocol.WriteHealthResponse(conn, protocol.HealthStatusReady, diskFree); err != nil {
		logger.Error("writing health response", "error", err)
	}
}

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

	// Lê agent name do TLS peer cert CN (para registrar control conn por agent)
	agentName := h.extractAgentName(conn, logger)
	if agentName == "" {
		agentName = conn.RemoteAddr().String() // fallback
	}

	// Registra control conn e mutex de write para este agent
	writeMu := &sync.Mutex{}
	cci := &ControlConnInfo{
		Conn:        conn,
		ConnectedAt: time.Now(),
		RemoteAddr:  conn.RemoteAddr().String(),
		KeepaliveS:  intervalSecs,
	}
	h.controlConns.Store(agentName, cci)
	h.controlConnsMu.Store(agentName, writeMu)
	defer h.controlConns.Delete(agentName)
	defer h.controlConnsMu.Delete(agentName)

	logger = logger.With("agent", agentName)
	logger.Info("control channel established",
		"keepalive_interval_s", intervalSecs,
		"read_timeout", readTimeout,
	)

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
				if ackCh, ok := ps.RotatePending.Load(streamIdx); ok {
					select {
					case ackCh.(chan struct{}) <- struct{}{}:
					default:
					}
					return false // encontrou e sinalizou, pode parar
				}
				return true // esta sessão não tem RotatePending para este idx, continua
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

		default:
			logger.Warn("control channel: unknown magic", "magic", string(magic[:]))
			return
		}
	}
}

// extractAgentName extrai o CN do certificado TLS peer para usar como agentName.
func (h *Handler) extractAgentName(conn net.Conn, logger *slog.Logger) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		cn := state.PeerCertificates[0].Subject.CommonName
		if cn != "" {
			return cn
		}
	}

	// Fallback: usa o remote address agrupado por IP (sem porta)
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	return host
}

// handleBackup processa uma sessão de backup completa.
func (h *Handler) handleBackup(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	// O magic "NBKP" já foi lido; ler restante do handshake (version + agent name + storage name)
	versionBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, versionBuf); err != nil {
		logger.Error("reading protocol version", "error", err)
		return
	}

	if versionBuf[0] != protocol.ProtocolVersion {
		logger.Error("unsupported protocol version", "version", versionBuf[0])
		protocol.WriteACK(conn, protocol.StatusReject, "unsupported protocol version", "")
		return
	}

	// Deadline para leitura do handshake (previne slowloris)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Lê agent name até '\n'
	agentName, err := readUntilNewline(conn)
	if err != nil {
		logger.Error("reading agent name", "error", err)
		return
	}

	// Lê storage name até '\n'
	storageName, err := readUntilNewline(conn)
	if err != nil {
		logger.Error("reading storage name", "error", err)
		return
	}

	// Lê backup name até '\n'
	backupName, err := readUntilNewline(conn)
	if err != nil {
		logger.Error("reading backup name", "error", err)
		return
	}

	// Protocolo v3+: Ler Client Version
	var clientVersion string
	if versionBuf[0] >= 0x03 {
		ver, err := readUntilNewline(conn)
		if err != nil {
			logger.Error("reading client version", "error", err)
			return
		}
		clientVersion = ver
	} else {
		clientVersion = "unknown (legacy)"
	}

	logger = logger.With("agent", agentName, "storage", storageName, "backup", backupName, "client_ver", clientVersion)
	logger.Info("backup handshake received")

	// Valida componentes de path contra traversal
	for _, v := range []struct{ val, field string }{
		{agentName, "agentName"},
		{storageName, "storageName"},
		{backupName, "backupName"},
	} {
		if err := validatePathComponent(v.val, v.field); err != nil {
			logger.Warn("invalid path component in handshake", "field", v.field, "value", v.val, "error", err)
			protocol.WriteACK(conn, protocol.StatusReject, fmt.Sprintf("invalid %s: %s", v.field, err), "")
			return
		}
	}

	// Valida identidade: agentName do protocolo deve coincidir com CN do certificado TLS
	certName := h.extractAgentName(conn, logger)
	if certName != "" && certName != agentName {
		logger.Warn("agent identity mismatch: protocol agentName does not match TLS certificate CN",
			"protocol_agent", agentName, "cert_cn", certName)
		protocol.WriteACK(conn, protocol.StatusReject,
			fmt.Sprintf("agent name %q does not match certificate CN %q", agentName, certName), "")
		return
	}

	// Busca storage nomeado
	conn.SetReadDeadline(time.Time{}) // limpa deadline do handshake
	storageInfo, ok := h.cfg.GetStorage(storageName)
	if !ok {
		logger.Warn("storage not found")
		protocol.WriteACK(conn, protocol.StatusStorageNotFound, fmt.Sprintf("storage %q not found", storageName), "")
		return
	}

	// Lock: por agent:storage:backup (permite backups simultâneos de entries diferentes)
	lockKey := agentName + ":" + storageName + ":" + backupName
	if _, loaded := h.locks.LoadOrStore(lockKey, true); loaded {
		logger.Warn("backup already in progress for agent")
		protocol.WriteACK(conn, protocol.StatusBusy, "backup already in progress", "")
		return
	}
	defer h.locks.Delete(lockKey)

	// Gera sessionID
	sessionID := generateSessionID()
	logger = logger.With("session", sessionID)

	// ACK GO
	if err := protocol.WriteACK(conn, protocol.StatusGo, "", sessionID); err != nil {
		logger.Error("writing ACK", "error", err)
		return
	}

	// Detecta modo: lê 1 byte discriminador
	// 0x00 = single-stream, 1-255 = MaxStreams de ParallelInit → modo paralelo
	br := bufio.NewReaderSize(conn, 8)
	modeByte := make([]byte, 1)
	if _, err := io.ReadFull(br, modeByte); err != nil {
		logger.Error("reading mode byte", "error", err)
		return
	}

	if modeByte[0] >= 1 {
		// Modo paralelo — o byte já lido é MaxStreams; lê ChunkSize (4B restantes)
		pi, err := protocol.ReadParallelInitAfterMaxStreams(br, modeByte[0])
		if err != nil {
			logger.Error("reading ParallelInit", "error", err)
			return
		}
		logger.Info("parallel mode detected", "maxStreams", pi.MaxStreams, "chunkSize", pi.ChunkSize)

		h.handleParallelBackup(ctx, conn, br, sessionID, agentName, storageName, backupName, clientVersion, storageInfo, pi, lockKey, logger)
		return
	}

	// Modo single-stream — byte 0x00 já consumido, br contém os dados

	// Prepara escrita atômica
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName, backupName)
	if err != nil {
		logger.Error("creating atomic writer", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	tmpFile, tmpPath, err := writer.TempFile()
	if err != nil {
		logger.Error("creating temp file", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Registra sessão parcial
	now := time.Now()
	session := &PartialSession{
		TmpPath:       tmpPath,
		AgentName:     agentName,
		StorageName:   storageName,
		BackupName:    backupName,
		BaseDir:       storageInfo.BaseDir,
		CreatedAt:     now,
		ClientVersion: clientVersion,
	}
	session.LastActivity.Store(now.UnixNano())
	h.sessions.Store(sessionID, session)
	defer h.sessions.Delete(sessionID) // limpa quando terminar com sucesso

	// Stream com SACK periódico — usa br em vez de conn para não perder dados bufferizados
	bytesReceived, err := h.receiveWithSACK(ctx, br, conn, tmpFile, tmpPath, session, logger)
	tmpFile.Close()

	if err != nil {
		logger.Error("receiving data stream", "error", err, "bytes", bytesReceived)
		// NÃO aborta o tmp — mantém para resume
		return
	}

	// Remove sessão — backup recebido com sucesso, resume não será necessário
	h.sessions.Delete(sessionID)

	// Validação do trailer e commit
	h.validateAndCommit(conn, writer, tmpPath, bytesReceived, storageInfo, logger)
}

// handleResume processa um pedido de resume do agent.
func (h *Handler) handleResume(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	resume, err := protocol.ReadResume(conn)
	if err != nil {
		logger.Error("reading resume frame", "error", err)
		return
	}

	logger = logger.With("session", resume.SessionID, "agent", resume.AgentName, "storage", resume.StorageName)
	logger.Info("resume request received")

	// Busca sessão parcial
	raw, ok := h.sessions.Load(resume.SessionID)
	if !ok {
		logger.Warn("session not found for resume")
		protocol.WriteResumeACK(conn, protocol.ResumeStatusNotFound, 0)
		return
	}
	session, ok := raw.(*PartialSession)
	if !ok {
		logger.Warn("resume: session is not a PartialSession (type mismatch)",
			"session", resume.SessionID)
		protocol.WriteResumeACK(conn, protocol.ResumeStatusNotFound, 0)
		return
	}

	// Valida agent e storage
	if session.AgentName != resume.AgentName || session.StorageName != resume.StorageName {
		logger.Warn("resume session mismatch",
			"expected_agent", session.AgentName, "got_agent", resume.AgentName,
			"expected_storage", session.StorageName, "got_storage", resume.StorageName)
		protocol.WriteResumeACK(conn, protocol.ResumeStatusNotFound, 0)
		return
	}

	// Verifica que o arquivo .tmp ainda existe
	fi, err := os.Stat(session.TmpPath)
	if err != nil {
		logger.Warn("tmp file gone for resume", "path", session.TmpPath, "error", err)
		h.sessions.Delete(resume.SessionID)
		protocol.WriteResumeACK(conn, protocol.ResumeStatusNotFound, 0)
		return
	}

	lastOffset := fi.Size()
	session.BytesWritten = lastOffset
	logger.Info("resume accepted", "last_offset", lastOffset)

	if err := protocol.WriteResumeACK(conn, protocol.ResumeStatusOK, uint64(lastOffset)); err != nil {
		logger.Error("writing resume ack", "error", err)
		return
	}

	// Lock: por agent:storage:backup
	lockKey := session.AgentName + ":" + session.StorageName + ":" + session.BackupName
	if _, loaded := h.locks.LoadOrStore(lockKey, true); loaded {
		logger.Warn("backup already in progress for agent during resume")
		return
	}
	defer h.locks.Delete(lockKey)

	// Reabrir tmp file para append
	tmpFile, err := os.OpenFile(session.TmpPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logger.Error("reopening tmp file for resume", "error", err)
		return
	}

	storageInfo, ok := h.cfg.GetStorage(session.StorageName)
	if !ok {
		logger.Error("storage not found during resume")
		tmpFile.Close()
		return
	}

	// Continua recebendo dados
	bytesReceived, err := h.receiveWithSACK(ctx, conn, conn, tmpFile, session.TmpPath, session, logger)
	tmpFile.Close()

	totalBytes := lastOffset + bytesReceived

	if err != nil {
		logger.Error("receiving resumed data", "error", err, "new_bytes", bytesReceived, "total", totalBytes)
		return
	}

	// Sucesso — remove sessão
	h.sessions.Delete(resume.SessionID)

	// Validação e commit
	writer, wErr := NewAtomicWriter(storageInfo.BaseDir, session.AgentName, session.BackupName)
	if wErr != nil {
		logger.Error("creating atomic writer for resume", "error", wErr)
		return
	}

	h.validateAndCommit(conn, writer, session.TmpPath, totalBytes, storageInfo, logger)
}

// receiveWithSACK lê dados do conn, escreve no tmpFile, e envia SACKs periódicos.
// Retorna o número de bytes recebidos nesta sessão (não o total do arquivo).
func (h *Handler) receiveWithSACK(ctx context.Context, reader io.Reader, sackWriter io.Writer, tmpFile *os.File, tmpPath string, session *PartialSession, logger *slog.Logger) (int64, error) {
	bufConn := bufio.NewReaderSize(reader, 256*1024)
	bufFile := bufio.NewWriterSize(tmpFile, 256*1024)

	var bytesReceived int64
	var lastSACK int64
	var sackErr atomic.Value // armazena erro de SACK para não bloquear

	// Sliding read deadline: reseta a cada read bem-sucedido.
	// Se a rede morrer silenciosamente (sem TCP RST), o read expirará em vez de travar para sempre.
	netConn, hasDeadline := sackWriter.(net.Conn)

	buf := make([]byte, 256*1024)
	for {
		if hasDeadline {
			netConn.SetReadDeadline(time.Now().Add(readInactivityTimeout))
		}
		n, readErr := bufConn.Read(buf)
		if n > 0 {
			if _, wErr := bufFile.Write(buf[:n]); wErr != nil {
				bufFile.Flush()
				return bytesReceived, fmt.Errorf("writing to tmp: %w", wErr)
			}
			bytesReceived += int64(n)
			session.BytesWritten += int64(n)
			session.LastActivity.Store(time.Now().UnixNano())
			h.TrafficIn.Add(int64(n))
			h.DiskWrite.Add(int64(n))

			// Envia SACK a cada sackInterval bytes
			if bytesReceived-lastSACK >= sackInterval {
				if fErr := bufFile.Flush(); fErr != nil {
					return bytesReceived, fmt.Errorf("flushing before sack: %w", fErr)
				}
				totalWritten := session.BytesWritten
				if sErr := protocol.WriteSACK(sackWriter, uint64(totalWritten)); sErr != nil {
					sackErr.Store(sErr)
					logger.Warn("failed to send SACK", "error", sErr, "offset", totalWritten)
				} else {
					logger.Debug("SACK sent", "offset", totalWritten)
				}
				lastSACK = bytesReceived
			}
		}

		if readErr != nil {
			// Flush antes de retornar
			if fErr := bufFile.Flush(); fErr != nil && readErr == io.EOF {
				return bytesReceived, fmt.Errorf("flushing file: %w", fErr)
			}
			if readErr == io.EOF {
				return bytesReceived, nil
			}
			return bytesReceived, readErr
		}
	}
}

// validateAndCommit valida o trailer, checksum e comita o backup.
func (h *Handler) validateAndCommit(conn net.Conn, writer *AtomicWriter, tmpPath string, totalBytes int64, storageInfo config.StorageInfo, logger *slog.Logger) {
	const trailerSize int64 = 4 + 32 + 8

	if totalBytes < trailerSize {
		logger.Error("received data too small", "bytes", totalBytes)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Lê o trailer dos últimos 44 bytes do arquivo
	trailer, err := readTrailerFromFile(tmpPath, trailerSize)
	if err != nil {
		logger.Error("reading trailer from file", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Trunca o arquivo para remover o trailer (mantém apenas os dados)
	dataSize := totalBytes - trailerSize
	if err := os.Truncate(tmpPath, dataSize); err != nil {
		logger.Error("truncating temp file", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Calcula SHA-256 dos dados (sem trailer)
	serverChecksum, err := hashFile(tmpPath)
	if err != nil {
		logger.Error("computing server checksum", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Compara checksums
	if serverChecksum != trailer.Checksum {
		logger.Error("checksum mismatch",
			"client", fmt.Sprintf("%x", trailer.Checksum),
			"server", fmt.Sprintf("%x", serverChecksum),
		)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusChecksumMismatch)
		return
	}

	// Commit (rename atômico)
	finalPath, err := writer.Commit(tmpPath)
	if err != nil {
		logger.Error("committing backup", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Rotação
	if err := Rotate(writer.AgentDir(), storageInfo.MaxBackups); err != nil {
		logger.Warn("rotation failed", "error", err)
	}

	logger.Info("backup committed",
		"path", finalPath,
		"bytes", dataSize,
		"checksum", fmt.Sprintf("%x", serverChecksum),
	)

	protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
}

// validateAndCommitWithTrailer valida e comita um backup paralelo.
// Diferente de validateAndCommit, o Trailer já foi recebido separadamente
// pela conn de controle (não embutido no arquivo). O arquivo contém apenas dados.
func (h *Handler) validateAndCommitWithTrailer(conn net.Conn, writer *AtomicWriter, tmpPath string, totalBytes int64, trailer *protocol.Trailer, serverChecksum [32]byte, storageInfo config.StorageInfo, logger *slog.Logger) {
	if totalBytes == 0 {
		logger.Error("no data received")
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Compara checksums
	if serverChecksum != trailer.Checksum {
		logger.Error("checksum mismatch",
			"client", fmt.Sprintf("%x", trailer.Checksum),
			"server", fmt.Sprintf("%x", serverChecksum),
		)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusChecksumMismatch)
		return
	}

	// Verifica tamanho
	if uint64(totalBytes) != trailer.Size {
		logger.Error("size mismatch",
			"client", trailer.Size,
			"server", totalBytes,
		)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Commit (rename atômico)
	finalPath, err := writer.Commit(tmpPath)
	if err != nil {
		logger.Error("committing backup", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Rotação
	if err := Rotate(writer.AgentDir(), storageInfo.MaxBackups); err != nil {
		logger.Warn("rotation failed", "error", err)
	}

	logger.Info("backup committed",
		"path", finalPath,
		"bytes", totalBytes,
		"checksum", fmt.Sprintf("%x", serverChecksum),
	)

	protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
}

// maxHandshakeFieldLen é o comprimento máximo permitido para campos do handshake
// (agentName, storageName, backupName, clientVersion).
const maxHandshakeFieldLen = 512

// readUntilNewline lê bytes até encontrar '\n', retornando a string sem o delimitador.
// Limitado a maxHandshakeFieldLen bytes para prevenir slowloris/OOM.
func readUntilNewline(conn net.Conn) (string, error) {
	var buf []byte
	oneByte := make([]byte, 1)
	for {
		if _, err := io.ReadFull(conn, oneByte); err != nil {
			return "", err
		}
		if oneByte[0] == '\n' {
			break
		}
		buf = append(buf, oneByte[0])
		if len(buf) > maxHandshakeFieldLen {
			return "", fmt.Errorf("handshake field exceeds max length %d", maxHandshakeFieldLen)
		}
	}
	return string(buf), nil
}

// readTrailerFromFile lê os últimos trailerSize bytes do arquivo e parseia como Trailer.
func readTrailerFromFile(path string, trailerSize int64) (*protocol.Trailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file for trailer: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stating file for trailer: %w", err)
	}

	offset := fi.Size() - trailerSize
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to trailer: %w", err)
	}

	return protocol.ReadTrailer(f)
}

// hashFile calcula o SHA-256 do conteúdo completo do arquivo.
func hashFile(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		var zero [32]byte
		return zero, fmt.Errorf("opening file for hash: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		var zero [32]byte
		return zero, fmt.Errorf("hashing file: %w", err)
	}

	var checksum [32]byte
	copy(checksum[:], h.Sum(nil))
	return checksum, nil
}

// generateSessionID gera um UUID v4 simples para identificar sessões de backup.
func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CleanupExpiredSessions remove sessões parciais expiradas e seus arquivos .tmp.
// O critério de expiração é baseado em LastActivity (último I/O bem-sucedido),
// não em CreatedAt, para evitar matar sessões ativas com backups grandes.
func CleanupExpiredSessions(sessions *sync.Map, ttl time.Duration, logger *slog.Logger) {
	sessions.Range(func(key, value any) bool {
		switch s := value.(type) {
		case *PartialSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			if time.Since(lastAct) > ttl {
				logger.Info("cleaning expired session",
					"session", key,
					"agent", s.AgentName,
					"storage", s.StorageName,
					"age", time.Since(s.CreatedAt).Round(time.Second),
					"idle", time.Since(lastAct).Round(time.Second),
				)
				os.Remove(s.TmpPath)
				sessions.Delete(key)
			}
		case *ParallelSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			if time.Since(lastAct) > ttl {
				logger.Info("cleaning expired parallel session",
					"session", key,
					"agent", s.AgentName,
					"storage", s.StorageName,
					"age", time.Since(s.CreatedAt).Round(time.Second),
					"idle", time.Since(lastAct).Round(time.Second),
				)
				s.Assembler.Cleanup()
				sessions.Delete(key)
			}
		}
		return true
	})
}

// ParallelSession rastreia uma sessão de backup com streams paralelos.
type ParallelSession struct {
	SessionID         string
	Assembler         *ChunkAssembler
	Writer            *AtomicWriter
	StorageInfo       config.StorageInfo
	AgentName         string
	StorageName       string
	BackupName        string
	StreamConns       sync.Map // streamIndex (uint8) → net.Conn
	StreamOffsets     sync.Map // streamIndex (uint8) → *int64 (completed bytes, atômico)
	StreamTrafficIn   sync.Map // streamIndex (uint8) → *atomic.Int64 (bytes recebidos no intervalo, para stats por stream)
	StreamTickBytes   sync.Map // streamIndex (uint8) → int64 (bytes observados no último tick do stats reporter)
	StreamLastAct     sync.Map // streamIndex (uint8) → *atomic.Int64 (UnixNano último I/O deste stream)
	StreamCancels     sync.Map // streamIndex (uint8) → context.CancelFunc (cancela goroutine anterior em re-join)
	StreamSlowSince   sync.Map // streamIndex (uint8) → time.Time (início de degradação contínua)
	StreamLastReset   sync.Map // streamIndex (uint8) → time.Time (último flow rotation deste stream)
	StreamConnectedAt sync.Map // streamIndex (uint8) → time.Time (timestamp do último connect/re-join)
	StreamReconnects  sync.Map // streamIndex (uint8) → *atomic.Int32 (reconexões acumuladas por stream)
	RotatePending     sync.Map // streamIndex (uint8) → chan struct{} (sinalizada por ControlRotateACK)
	MaxStreams        uint8
	ChunkSize         uint32
	StreamWg          sync.WaitGroup // barreira para todos os streams
	StreamReady       chan struct{}  // fechado quando o primeiro stream conecta
	streamReadyOnce   sync.Once      // garante close único do StreamReady
	Done              chan struct{}  // sinaliza conclusão
	CreatedAt         time.Time
	LastActivity      atomic.Int64  // UnixNano do último I/O bem-sucedido
	DiskWriteBytes    atomic.Int64  // Total de bytes escritos em disco nesta sessão
	TotalObjects      atomic.Uint32 // Total de objetos a enviar (recebido via ControlProgress)
	ObjectsSent       atomic.Uint32 // Objetos já enviados (recebido via ControlProgress)
	WalkComplete      atomic.Int32  // 1 = prescan concluído, total confiável (via ControlProgress)
	ClientVersion     string        // Versão do client (protocolo v3+)
}

// handleParallelBackup processa um backup paralelo.
// A conexão primária é usada apenas como canal de controle (Trailer + FinalACK).
// Todos os dados são recebidos via streams secundários (ParallelJoin).
func (h *Handler) handleParallelBackup(ctx context.Context, conn net.Conn, br io.Reader, sessionID, agentName, storageName, backupName, clientVersion string, storageInfo config.StorageInfo, pi *protocol.ParallelInit, lockKey string, logger *slog.Logger) {
	defer h.locks.Delete(lockKey)

	logger = logger.With("session", sessionID, "mode", "parallel", "maxStreams", pi.MaxStreams)
	logger.Info("starting parallel backup session")

	// Prepara escrita atômica
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName, backupName)
	if err != nil {
		logger.Error("creating atomic writer", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Cria assembler para staging de chunks (configurável por storage)
	assembler, err := NewChunkAssemblerWithOptions(sessionID, writer.AgentDir(), logger, ChunkAssemblerOptions{
		Mode:            storageInfo.AssemblerMode,
		PendingMemLimit: storageInfo.AssemblerPendingMemRaw,
	})
	if err != nil {
		logger.Error("creating chunk assembler", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}
	defer assembler.Cleanup()

	// Registra sessão paralela para que handleParallelJoin possa encontrar
	now := time.Now()
	pSession := &ParallelSession{
		SessionID:     sessionID,
		Assembler:     assembler,
		Writer:        writer,
		StorageInfo:   storageInfo,
		AgentName:     agentName,
		StorageName:   storageName,
		BackupName:    backupName,
		ClientVersion: clientVersion,
		MaxStreams:    pi.MaxStreams,
		ChunkSize:     pi.ChunkSize,
		StreamReady:   make(chan struct{}),
		Done:          make(chan struct{}),
		CreatedAt:     now,
	}
	pSession.LastActivity.Store(now.UnixNano())
	h.sessions.Store(sessionID, pSession)
	defer h.sessions.Delete(sessionID)

	// Conn primária é control-only: não recebe dados de stream 0 aqui.
	// Todos os N streams de dados conectam via ParallelJoin (handleParallelJoin).

	// Espera pelo menos 1 stream conectar (via canal StreamReady),
	// depois espera todos terminarem via StreamWg.
	select {
	case <-pSession.StreamReady:
		// Pelo menos 1 stream conectou — espera todos finalizarem.
	case <-ctx.Done():
		logger.Error("context cancelled waiting for streams")
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	case <-time.After(5 * time.Minute):
		logger.Error("timeout waiting for streams to connect")
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	pSession.StreamWg.Wait()
	logger.Info("all parallel streams complete")

	// Finaliza o assembler (flush + close)
	assembledPath, totalBytes, err := assembler.Finalize()
	if err != nil {
		logger.Error("finalizing assembly", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Lê Trailer direto da conn primária (sem ChunkHeader framing).
	// O agent envia o Trailer diretamente após todos os senders finalizarem.
	logger.Info("parallel assembly complete, waiting for trailer", "totalBytes", totalBytes)

	// Set read deadline para a conn primária enquanto espera o Trailer
	conn.SetReadDeadline(time.Now().Add(readInactivityTimeout))
	trailer, err := protocol.ReadTrailer(br)
	if err != nil {
		logger.Error("reading trailer from primary conn", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}
	conn.SetReadDeadline(time.Time{}) // limpa deadline

	// Validação do checksum e commit
	serverChecksum, err := assembler.Checksum()
	if err != nil {
		logger.Error("getting assembled checksum", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}
	h.validateAndCommitWithTrailer(conn, writer, assembledPath, totalBytes, trailer, serverChecksum, storageInfo, logger)
}

// receiveParallelStream recebe dados de um stream paralelo usando ChunkHeader framing.
// Cada chunk é precedido por um ChunkHeader (8B: GlobalSeq uint32 + Length uint32).
// Os dados são escritos incrementalmente no assembler, que decide se escreve direto
// no arquivo final (in-order) ou bufferiza temporariamente (out-of-order).
// O stream termina com EOF (CloseWrite do agent), io.EOF no reader, ou cancelamento do ctx.
// Atualiza StreamOffsets atomicamente após cada chunk completo para suporte a resume.
func (h *Handler) receiveParallelStream(ctx context.Context, conn net.Conn, reader io.Reader, sackWriter io.Writer, streamIndex uint8, session *ParallelSession, logger *slog.Logger) (int64, error) {
	var bytesReceived int64
	var localChunkSeq uint32

	// Inicializa ou recupera o ponteiro de offset para este stream
	offsetPtr := new(int64)
	if existing, loaded := session.StreamOffsets.LoadOrStore(streamIndex, offsetPtr); loaded {
		offsetPtr = existing.(*int64)
		bytesReceived = atomic.LoadInt64(offsetPtr)
		logger.Info("resuming stream from offset", "stream", streamIndex, "offset", bytesReceived)
	}

	for {
		// Verifica cancelamento do contexto (ex: re-join de outro stream com mesmo index)
		select {
		case <-ctx.Done():
			logger.Info("stream context cancelled", "stream", streamIndex, "bytes", bytesReceived)
			return bytesReceived, ctx.Err()
		default:
		}

		// Sliding read deadline com timeout curto para streams paralelos.
		// Quanto menor o deadline, mais rápido o agent detecta a falha e reconecta.
		conn.SetReadDeadline(time.Now().Add(streamReadDeadline))

		// Lê ChunkHeader (8 bytes: GlobalSeq + Length)
		hdr, err := protocol.ReadChunkHeader(reader)
		if err != nil {
			if err == io.EOF || err.Error() == "reading chunk header seq: EOF" {
				break
			}
			return bytesReceived, fmt.Errorf("reading chunk header from stream %d: %w", streamIndex, err)
		}

		// Escreve incrementalmente no assembler
		if err := session.Assembler.WriteChunk(hdr.GlobalSeq, io.LimitReader(reader, int64(hdr.Length)), int64(hdr.Length)); err != nil {
			return bytesReceived, fmt.Errorf("writing chunk seq %d to assembler: %w", hdr.GlobalSeq, err)
		}

		nowNano := time.Now().UnixNano()
		bytesReceived += int64(hdr.Length) + protocol.ChunkHeaderSize
		session.LastActivity.Store(nowNano)
		h.TrafficIn.Add(int64(hdr.Length))
		h.DiskWrite.Add(int64(hdr.Length))
		session.DiskWriteBytes.Add(int64(hdr.Length))

		// Per-stream stats: incrementa tráfego e atualiza last activity
		if counter, ok := session.StreamTrafficIn.Load(streamIndex); ok {
			counter.(*atomic.Int64).Add(int64(hdr.Length))
		}
		if lastAct, ok := session.StreamLastAct.Load(streamIndex); ok {
			lastAct.(*atomic.Int64).Store(nowNano)
		}

		// Atualiza offset atômico — usado por handleParallelJoin para resume
		atomic.StoreInt64(offsetPtr, bytesReceived)

		// Envia ChunkSACK com write timeout para não bloquear se a conn está morta
		if netConn, ok := sackWriter.(net.Conn); ok {
			netConn.SetWriteDeadline(time.Now().Add(sackWriteTimeout))
		}
		localChunkSeq++
		if sErr := protocol.WriteChunkSACK(sackWriter, streamIndex, localChunkSeq, uint64(bytesReceived)); sErr != nil {
			logger.Warn("failed to send ChunkSACK", "error", sErr, "stream", streamIndex, "seq", localChunkSeq)
		} else {
			logger.Debug("ChunkSACK sent", "stream", streamIndex, "globalSeq", hdr.GlobalSeq, "offset", bytesReceived)
		}
	}

	return bytesReceived, nil
}

// handleParallelJoin processa uma conexão secundária de ParallelJoin.
// Suporta re-join: se o stream já foi conectado antes, cancela a goroutine anterior,
// fecha a conexão antiga, e inicia nova goroutine usando o mesmo slot do StreamWg.
func (h *Handler) handleParallelJoin(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	// O magic "PJIN" já foi lido pelo HandleConnection
	pj, err := protocol.ReadParallelJoin(conn)
	if err != nil {
		logger.Error("reading ParallelJoin", "error", err)
		return
	}

	logger = logger.With("session", pj.SessionID, "stream", pj.StreamIndex)
	logger.Info("parallel join request received")

	// Busca sessão paralela
	raw, ok := h.sessions.Load(pj.SessionID)
	if !ok {
		logger.Warn("parallel session not found")
		protocol.WriteParallelACK(conn, protocol.ParallelStatusNotFound, 0)
		return
	}

	pSession, ok := raw.(*ParallelSession)
	if !ok {
		logger.Warn("session is not a parallel session")
		protocol.WriteParallelACK(conn, protocol.ParallelStatusNotFound, 0)
		return
	}

	// Valida stream index
	if pj.StreamIndex >= pSession.MaxStreams {
		logger.Warn("stream index exceeds max", "maxStreams", pSession.MaxStreams)
		protocol.WriteParallelACK(conn, protocol.ParallelStatusFull, 0)
		return
	}

	// --- Cancelamento da goroutine anterior (proteção contra goroutine leak) ---
	// Se este stream já foi conectado antes (re-join), cancela o contexto da goroutine
	// anterior para que ela saia imediatamente em vez de esperar o read timeout.
	if oldCancel, loaded := pSession.StreamCancels.Load(pj.StreamIndex); loaded {
		logger.Info("cancelling previous stream goroutine for re-join", "stream", pj.StreamIndex)
		oldCancel.(context.CancelFunc)()
	}

	// Fecha a conexão anterior para desbloquear reads pendentes
	if oldConn, loaded := pSession.StreamConns.Load(pj.StreamIndex); loaded {
		oldConn.(net.Conn).Close()
	}

	// Verifica se é re-join (stream já conectou antes) — resume offset
	var lastOffset uint64
	if raw, ok := pSession.StreamOffsets.Load(pj.StreamIndex); ok {
		lastOffset = uint64(atomic.LoadInt64(raw.(*int64)))
		logger.Info("parallel stream re-join (resume)", "lastOffset", lastOffset)
	}

	// ACK OK com lastOffset para negociação de resume
	if err := protocol.WriteParallelACK(conn, protocol.ParallelStatusOK, lastOffset); err != nil {
		logger.Error("writing ParallelACK", "error", err)
		return
	}

	// Registra conexão do stream
	pSession.StreamConns.Store(pj.StreamIndex, conn)

	// Atualiza uptime e reconnects do stream
	// No primeiro connect, StreamReconnects não existe — LoadOrStore cria com zero.
	// Em re-joins subsequentes, incrementa o contador.
	if rcRaw, loaded := pSession.StreamReconnects.LoadOrStore(pj.StreamIndex, &atomic.Int32{}); loaded {
		rcRaw.(*atomic.Int32).Add(1)
	}
	pSession.StreamConnectedAt.Store(pj.StreamIndex, time.Now())

	// Inicializa counters per-stream para stats
	nowNano := time.Now().UnixNano()
	trafficCounter := &atomic.Int64{}
	pSession.StreamTrafficIn.LoadOrStore(pj.StreamIndex, trafficCounter)
	lastActCounter := &atomic.Int64{}
	lastActCounter.Store(nowNano)
	pSession.StreamLastAct.LoadOrStore(pj.StreamIndex, lastActCounter)

	// Cria contexto cancelável para esta goroutine específica.
	// Será cancelado se outro re-join chegar para o mesmo stream index.
	streamCtx, streamCancel := context.WithCancel(ctx)
	pSession.StreamCancels.Store(pj.StreamIndex, streamCancel)

	// StreamWg.Add(1) SEMPRE — cada goroutine (inclusive a cancelada por re-join)
	// faz exatamente um Done(). Com cancelamento via contexto, a goroutine antiga
	// sai rapidamente, então o counter fica consistente:
	//   join:    Add(1) → counter=1 → Done() → counter=0
	//   re-join: Add(1) → counter=2 → old Done() → counter=1 → new Done() → counter=0
	pSession.StreamWg.Add(1)

	// Sinaliza que pelo menos 1 stream conectou APÓS incrementar o WaitGroup.
	// Isso evita race onde handleParallelBackup faz Wait() com counter ainda 0.
	pSession.streamReadyOnce.Do(func() { close(pSession.StreamReady) })

	// Recebe dados do stream com ChunkHeader framing
	bytesReceived, err := h.receiveParallelStream(streamCtx, conn, conn, conn, pj.StreamIndex, pSession, logger)
	pSession.StreamWg.Done()

	if err != nil {
		// context.Canceled é esperado em re-join — não é um erro real
		if ctx.Err() == nil && streamCtx.Err() == context.Canceled {
			logger.Info("parallel stream replaced by re-join", "bytes", bytesReceived)
			return
		}
		logger.Error("receiving parallel stream", "error", err, "bytes", bytesReceived)
		return
	}

	logger.Info("parallel stream complete", "bytes", bytesReceived)
}
