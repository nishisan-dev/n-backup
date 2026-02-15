// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// sackInterval define a cada quantos bytes o server envia um SACK.
const sackInterval = 64 * 1024 * 1024 // 64MB

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
	TmpPath      string
	BytesWritten int64
	AgentName    string
	StorageName  string
	BackupName   string
	BaseDir      string
	CreatedAt    time.Time
	LastActivity atomic.Int64 // UnixNano do último I/O bem-sucedido
}

// Handler processa conexões individuais de backup.
type Handler struct {
	cfg      *config.ServerConfig
	logger   *slog.Logger
	locks    *sync.Map // Mapa de locks por "agent:storage"
	sessions *sync.Map // Mapa de sessões parciais por sessionID

	// Métricas observáveis pelo stats reporter
	TrafficIn   atomic.Int64 // bytes recebidos da rede (acumulado desde último reset)
	DiskWrite   atomic.Int64 // bytes escritos em disco (acumulado desde último reset)
	ActiveConns atomic.Int32 // conexões ativas no momento
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
			secs := interval.Seconds()
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

			// Per-stream stats (configurável)
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
			// Swap-and-reset: lê bytes do intervalo e zera
			bytes := counter.Swap(0)
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

	logger = logger.With("agent", agentName, "storage", storageName, "backup", backupName)
	logger.Info("backup handshake received")

	// Busca storage nomeado
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

		h.handleParallelBackup(ctx, conn, br, sessionID, agentName, storageName, backupName, storageInfo, pi, lockKey, logger)
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
		TmpPath:     tmpPath,
		AgentName:   agentName,
		StorageName: storageName,
		BackupName:  backupName,
		BaseDir:     storageInfo.BaseDir,
		CreatedAt:   now,
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
	session := raw.(*PartialSession)

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
func (h *Handler) validateAndCommitWithTrailer(conn net.Conn, writer *AtomicWriter, tmpPath string, totalBytes int64, trailer *protocol.Trailer, storageInfo config.StorageInfo, logger *slog.Logger) {
	if totalBytes == 0 {
		logger.Error("no data received")
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Calcula SHA-256 dos dados (arquivo inteiro = dados puros)
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

// readUntilNewline lê bytes até encontrar '\n', retornando a string sem o delimitador.
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
	SessionID       string
	Assembler       *ChunkAssembler
	Writer          *AtomicWriter
	StorageInfo     config.StorageInfo
	AgentName       string
	StorageName     string
	BackupName      string
	StreamConns     sync.Map // streamIndex (uint8) → net.Conn
	StreamOffsets   sync.Map // streamIndex (uint8) → *int64 (completed bytes, atômico)
	StreamTrafficIn sync.Map // streamIndex (uint8) → *atomic.Int64 (bytes recebidos no intervalo, para stats por stream)
	StreamLastAct   sync.Map // streamIndex (uint8) → *atomic.Int64 (UnixNano último I/O deste stream)
	StreamCancels   sync.Map // streamIndex (uint8) → context.CancelFunc (cancela goroutine anterior em re-join)
	MaxStreams      uint8
	ChunkSize       uint32
	StreamWg        sync.WaitGroup // barreira para todos os streams
	StreamReady     chan struct{}  // fechado quando o primeiro stream conecta
	streamReadyOnce sync.Once      // garante close único do StreamReady
	Done            chan struct{}  // sinaliza conclusão
	CreatedAt       time.Time
	LastActivity    atomic.Int64 // UnixNano do último I/O bem-sucedido
}

// handleParallelBackup processa um backup paralelo.
// A conexão primária é usada apenas como canal de controle (Trailer + FinalACK).
// Todos os dados são recebidos via streams secundários (ParallelJoin).
func (h *Handler) handleParallelBackup(ctx context.Context, conn net.Conn, br io.Reader, sessionID, agentName, storageName, backupName string, storageInfo config.StorageInfo, pi *protocol.ParallelInit, lockKey string, logger *slog.Logger) {
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

	// Cria assembler para staging de chunks
	assembler, err := NewChunkAssembler(sessionID, writer.AgentDir(), logger)
	if err != nil {
		logger.Error("creating chunk assembler", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}
	defer assembler.Cleanup()

	// Registra sessão paralela para que handleParallelJoin possa encontrar
	now := time.Now()
	pSession := &ParallelSession{
		SessionID:   sessionID,
		Assembler:   assembler,
		Writer:      writer,
		StorageInfo: storageInfo,
		AgentName:   agentName,
		StorageName: storageName,
		BackupName:  backupName,
		MaxStreams:  pi.MaxStreams,
		ChunkSize:   pi.ChunkSize,
		StreamReady: make(chan struct{}),
		Done:        make(chan struct{}),
		CreatedAt:   now,
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
	h.validateAndCommitWithTrailer(conn, writer, assembledPath, totalBytes, trailer, storageInfo, logger)
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
		bytesReceived += int64(hdr.Length)
		session.LastActivity.Store(nowNano)
		h.TrafficIn.Add(int64(hdr.Length))
		h.DiskWrite.Add(int64(hdr.Length))

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
	isReJoin := false
	if oldCancel, loaded := pSession.StreamCancels.Load(pj.StreamIndex); loaded {
		isReJoin = true
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

	// Inicializa counters per-stream para stats
	nowNano := time.Now().UnixNano()
	trafficCounter := &atomic.Int64{}
	pSession.StreamTrafficIn.LoadOrStore(pj.StreamIndex, trafficCounter)
	lastActCounter := &atomic.Int64{}
	lastActCounter.Store(nowNano)
	pSession.StreamLastAct.LoadOrStore(pj.StreamIndex, lastActCounter)

	// Sinaliza que pelo menos 1 stream conectou
	pSession.streamReadyOnce.Do(func() { close(pSession.StreamReady) })

	// Cria contexto cancelável para esta goroutine específica.
	// Será cancelado se outro re-join chegar para o mesmo stream index.
	streamCtx, streamCancel := context.WithCancel(ctx)
	pSession.StreamCancels.Store(pj.StreamIndex, streamCancel)

	// StreamWg.Add(1) apenas na PRIMEIRA conexão deste stream.
	// Em re-join, a goroutine anterior vai fazer Done() quando detectar o cancelamento,
	// compensando o Add(1) original. A nova goroutine herda o mesmo slot.
	if !isReJoin {
		pSession.StreamWg.Add(1)
	}

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
