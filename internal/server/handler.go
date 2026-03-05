// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler.go contém as estruturas e constantes centrais do Handler de backup,
// além do ponto de entrada HandleConnection que despacha para os handlers
// especializados em cada arquivo dedicado:
//
//   handler_health.go       — health check (PING)
//   handler_single.go       — backup single-stream e resume (NBKP, RSME)
//   handler_parallel.go     — backup paralelo e join (PJIN), ParallelSession
//   handler_control.go      — control channel e flow rotation (CTRL)
//   handler_observability.go — métricas, snapshots, stats reporter
//   handler_storage.go      — storage scan, cache e cleanup de sessões

package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// ---------------------------------------------------------------------------
// Constantes de tunning do Handler
// ---------------------------------------------------------------------------

// sackInterval define a cada quantos bytes o server envia um SACK.
// 4MB reduz overhead de ACK/flush em WAN sem atrasar demais o progresso de resume.
const sackInterval = 4 * 1024 * 1024 // 4MB

// singleStreamIOBufferSize é o tamanho dos buffers do caminho single-stream.
// 1MB reduz syscalls e melhora vazão sustentada em transferências grandes.
const singleStreamIOBufferSize = 1 * 1024 * 1024 // 1MB

// readInactivityTimeout é o tempo máximo de inatividade na leitura de dados (single-stream).
// Se expirar, a conexão é considerada morta e a goroutine é liberada.
const readInactivityTimeout = 90 * time.Second

// streamReadDeadline é o deadline de read para streams paralelos.
// Menor que readInactivityTimeout porque streams paralelos têm reconexão automática:
// quanto mais rápido detectar a falha, mais rápido o agent pode reconectar.
const streamReadDeadline = 30 * time.Second

// sackWriteTimeout é o deadline de write para envio de SACKs/ChunkSACKs.
const sackWriteTimeout = 10 * time.Second

// ---------------------------------------------------------------------------
// Tipos core
// ---------------------------------------------------------------------------

// PartialSession rastreia um backup parcial single-stream para resume.
type PartialSession struct {
	TmpPath         string
	BytesWritten    atomic.Int64
	AgentName       string
	StorageName     string
	BackupName      string
	BaseDir         string
	CreatedAt       time.Time
	LastActivity    atomic.Int64 // UnixNano do último I/O bem-sucedido
	ClientVersion   string       // Versão do client (protocolo v3+)
	CompressionMode string       // gzip | zst
}

// Handler processa conexões individuais de backup.
// Cada conexão é despachada por HandleConnection para o handler especializado
// de acordo com o magic de 4 bytes recebido.
type Handler struct {
	cfg      *config.ServerConfig
	logger   *slog.Logger
	locks    *sync.Map // Mapa de locks por "agent:storage:backup"
	sessions *sync.Map // Mapa de sessões (PartialSession ou ParallelSession) por sessionID

	// chunkBuffer é o buffer de chunks em memória global (nil quando desabilitado).
	chunkBuffer *ChunkBuffer

	// Control channel registry: agentName → *ControlConnInfo
	// Registrado em handleControlChannel, usado por evaluateFlowRotation
	// para enviar ControlRotate graceful, e por ConnectedAgents para observabilidade.
	controlConns   sync.Map // agentName (string) → *ControlConnInfo
	controlConnsMu sync.Map // agentName (string) → *sync.Mutex (protege writes na conn)

	// Métricas observáveis pelo stats reporter
	TrafficIn   atomic.Int64 // bytes recebidos da rede (acumulado desde último reset)
	DiskWrite   atomic.Int64 // bytes escritos em disco (acumulado desde último reset)
	ActiveConns atomic.Int32 // conexões ativas no momento

	// Events store para observabilidade e persistência (nil quando WebUI desabilitada).
	Events *observability.EventStore

	// SessionHistory mantém histórico de sessões finalizadas (nil quando WebUI desabilitada).
	SessionHistory *observability.SessionHistoryStore

	// ActiveSessionHistory mantém snapshots periódicos de sessões ativas (nil quando WebUI desabilitada).
	ActiveSessionHistory *observability.ActiveSessionStore

	// storageCache mantém snapshot cacheado de StorageUsage, atualizado por StartStorageScanner.
	// Evita syscall.Statfs + filepath.WalkDir a cada request HTTP.
	storageCache atomic.Value // []observability.StorageUsage

	// syncRunning guarda o estado do sync retroativo para evitar execuções concorrentes.
	syncRunning atomic.Bool

	// lastSyncResult armazena o resultado da última sincronização retroativa.
	// Consultável via WebUI/API para visibilidade operacional.
	lastSyncResult atomic.Value // *SyncStorageResult

	// syncProgress rastreia progresso em tempo real do sync retroativo.
	// Campos atômicos para leitura lock-free pelo endpoint HTTP.
	syncProgress SyncProgress
}

// ControlConnInfo armazena metadata de um control channel conectado.
type ControlConnInfo struct {
	Conn          net.Conn
	ConnectedAt   time.Time
	RemoteAddr    string
	KeepaliveS    uint32
	ClientVersion string
	Stats         atomic.Value // *observability.AgentStats
}

// ---------------------------------------------------------------------------
// Constructor e lifecycle
// ---------------------------------------------------------------------------

// NewHandler cria um novo Handler inicializado com config, logger e maps compartilhados.
func NewHandler(cfg *config.ServerConfig, logger *slog.Logger, locks *sync.Map, sessions *sync.Map) *Handler {
	return &Handler{
		cfg:         cfg,
		logger:      logger,
		locks:       locks,
		sessions:    sessions,
		chunkBuffer: NewChunkBuffer(cfg.ChunkBuffer, logger),
	}
}

// StartChunkBuffer inicia a goroutine de drenagem do buffer de chunks.
// Deve ser chamado uma vez após NewHandler, antes de aceitar conexões.
// É no-op quando o buffer está desabilitado.
func (h *Handler) StartChunkBuffer(ctx context.Context) {
	if h.chunkBuffer != nil {
		h.chunkBuffer.StartDrainer(ctx)
	}
}

// ---------------------------------------------------------------------------
// Control channel registry helpers
// ---------------------------------------------------------------------------

// registerControlConn publica a conexão de controle atual para um agent.
func (h *Handler) registerControlConn(agentName string, cci *ControlConnInfo, writeMu *sync.Mutex) {
	h.controlConns.Store(agentName, cci)
	h.controlConnsMu.Store(agentName, writeMu)
}

// unregisterControlConn remove o registro somente se ele ainda aponta para a
// mesma conexão/mutex que esta goroutine instalou. Isso evita que um disconnect
// tardio apague uma reconexão mais recente do mesmo agent.
func (h *Handler) unregisterControlConn(agentName string, cci *ControlConnInfo, writeMu *sync.Mutex) {
	h.controlConns.CompareAndDelete(agentName, cci)
	h.controlConnsMu.CompareAndDelete(agentName, writeMu)
}

// ---------------------------------------------------------------------------
// Connection entry point
// ---------------------------------------------------------------------------

// HandleConnection processa uma conexão individual de backup.
// Lê os primeiros 4 bytes (magic) para determinar o tipo de sessão e
// despacha para o handler especializado correspondente.
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

// ---------------------------------------------------------------------------
// TLS helper
// ---------------------------------------------------------------------------

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
