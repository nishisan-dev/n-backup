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

// PartialSession rastreia um backup parcial para resume.
type PartialSession struct {
	TmpPath      string
	BytesWritten int64
	AgentName    string
	StorageName  string
	BaseDir      string
	CreatedAt    time.Time
}

// Handler processa conexões individuais de backup.
type Handler struct {
	cfg      *config.ServerConfig
	logger   *slog.Logger
	locks    *sync.Map // Mapa de locks por "agent:storage"
	sessions *sync.Map // Mapa de sessões parciais por sessionID
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

// HandleConnection processa uma conexão individual de backup.
func (h *Handler) HandleConnection(ctx context.Context, conn net.Conn) {
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

	logger = logger.With("agent", agentName, "storage", storageName)
	logger.Info("backup handshake received")

	// Busca storage nomeado
	storageInfo, ok := h.cfg.GetStorage(storageName)
	if !ok {
		logger.Warn("storage not found")
		protocol.WriteACK(conn, protocol.StatusStorageNotFound, fmt.Sprintf("storage %q not found", storageName), "")
		return
	}

	// Lock: por agent:storage (permite backups simultâneos de storages diferentes do mesmo agent)
	lockKey := agentName + ":" + storageName
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

	// Detecta extensão ParallelInit: peek 1 byte
	// Se valor estiver entre 1-8, é MaxStreams de ParallelInit → modo paralelo
	br := bufio.NewReaderSize(conn, 8)
	peek, err := br.Peek(1)
	if err != nil {
		logger.Error("peeking for parallel init", "error", err)
		return
	}

	if peek[0] >= 1 && peek[0] <= 8 {
		// Modo paralelo — lê ParallelInit completo
		pi, err := protocol.ReadParallelInit(br)
		if err != nil {
			logger.Error("reading ParallelInit", "error", err)
			return
		}
		logger.Info("parallel mode detected", "maxStreams", pi.MaxStreams, "chunkSize", pi.ChunkSize)

		h.handleParallelBackup(ctx, conn, br, sessionID, agentName, storageName, storageInfo, pi, lockKey, logger)
		return
	}

	// Modo single-stream (legacy) — usa br que já tem os dados bufferizados

	// Prepara escrita atômica
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName)
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
	session := &PartialSession{
		TmpPath:     tmpPath,
		AgentName:   agentName,
		StorageName: storageName,
		BaseDir:     storageInfo.BaseDir,
		CreatedAt:   time.Now(),
	}
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

	// Lock: por agent:storage
	lockKey := session.AgentName + ":" + session.StorageName
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
	writer, wErr := NewAtomicWriter(storageInfo.BaseDir, session.AgentName)
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

	buf := make([]byte, 256*1024)
	for {
		n, readErr := bufConn.Read(buf)
		if n > 0 {
			if _, wErr := bufFile.Write(buf[:n]); wErr != nil {
				bufFile.Flush()
				return bytesReceived, fmt.Errorf("writing to tmp: %w", wErr)
			}
			bytesReceived += int64(n)
			session.BytesWritten += int64(n)

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
		return
	}

	// Lê o trailer dos últimos 44 bytes do arquivo
	trailer, err := readTrailerFromFile(tmpPath, trailerSize)
	if err != nil {
		logger.Error("reading trailer from file", "error", err)
		writer.Abort(tmpPath)
		return
	}

	// Trunca o arquivo para remover o trailer (mantém apenas os dados)
	dataSize := totalBytes - trailerSize
	if err := os.Truncate(tmpPath, dataSize); err != nil {
		logger.Error("truncating temp file", "error", err)
		writer.Abort(tmpPath)
		return
	}

	// Calcula SHA-256 dos dados (sem trailer)
	serverChecksum, err := hashFile(tmpPath)
	if err != nil {
		logger.Error("computing server checksum", "error", err)
		writer.Abort(tmpPath)
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
func CleanupExpiredSessions(sessions *sync.Map, ttl time.Duration, logger *slog.Logger) {
	sessions.Range(func(key, value any) bool {
		session := value.(*PartialSession)
		if time.Since(session.CreatedAt) > ttl {
			logger.Info("cleaning expired session",
				"session", key,
				"agent", session.AgentName,
				"storage", session.StorageName,
				"age", time.Since(session.CreatedAt).Round(time.Second),
			)
			// Remove .tmp
			os.Remove(session.TmpPath)
			sessions.Delete(key)
		}
		return true
	})
}

// ParallelSession rastreia uma sessão de backup com streams paralelos.
type ParallelSession struct {
	SessionID   string
	Assembler   *ChunkAssembler
	Writer      *AtomicWriter
	StorageInfo config.StorageInfo
	AgentName   string
	StorageName string
	StreamConns sync.Map // streamIndex (uint8) → net.Conn
	MaxStreams  uint8
	ChunkSize   uint32
	Done        chan struct{} // sinaliza conclusão
}

// handleParallelBackup processa um backup paralelo.
// Recebe dados pela conexão primária (stream 0) + streams secundários via ParallelJoin.
func (h *Handler) handleParallelBackup(ctx context.Context, conn net.Conn, br io.Reader, sessionID, agentName, storageName string, storageInfo config.StorageInfo, pi *protocol.ParallelInit, lockKey string, logger *slog.Logger) {
	defer h.locks.Delete(lockKey)

	logger = logger.With("session", sessionID, "mode", "parallel", "maxStreams", pi.MaxStreams)
	logger.Info("starting parallel backup session")

	// Prepara escrita atômica
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName)
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
	pSession := &ParallelSession{
		SessionID:   sessionID,
		Assembler:   assembler,
		Writer:      writer,
		StorageInfo: storageInfo,
		AgentName:   agentName,
		StorageName: storageName,
		MaxStreams:  pi.MaxStreams,
		ChunkSize:   pi.ChunkSize,
		Done:        make(chan struct{}),
	}
	h.sessions.Store(sessionID, pSession)
	defer h.sessions.Delete(sessionID)

	// Abre chunk file para stream 0 e recebe dados
	chunkFile, _, err := assembler.ChunkFile(0)
	if err != nil {
		logger.Error("opening chunk file for stream 0", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Recebe dados do stream 0 com ChunkSACK
	bytesReceived, err := h.receiveParallelStream(ctx, br, conn, chunkFile, 0, pSession, logger)
	chunkFile.Close()

	if err != nil {
		logger.Error("receiving parallel stream 0", "error", err, "bytes", bytesReceived)
		return
	}

	logger.Info("stream 0 data received", "bytes", bytesReceived)

	// Monta o arquivo final
	assembledPath, totalBytes, err := assembler.Assemble()
	if err != nil {
		logger.Error("assembling chunks", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Validação do trailer e commit
	logger.Info("parallel assembly complete, awaiting trailer", "totalBytes", totalBytes)
	h.validateAndCommit(conn, writer, assembledPath, totalBytes, storageInfo, logger)
}

// receiveParallelStream recebe dados de um stream paralelo e envia ChunkSACKs.
func (h *Handler) receiveParallelStream(ctx context.Context, reader io.Reader, sackWriter io.Writer, chunkFile *os.File, streamIndex uint8, session *ParallelSession, logger *slog.Logger) (int64, error) {
	bufReader := bufio.NewReaderSize(reader, 256*1024)
	bufWriter := bufio.NewWriterSize(chunkFile, 256*1024)

	var bytesReceived int64
	var chunkSeq uint32

	buf := make([]byte, 256*1024)
	for {
		n, readErr := bufReader.Read(buf)
		if n > 0 {
			if _, wErr := bufWriter.Write(buf[:n]); wErr != nil {
				bufWriter.Flush()
				return bytesReceived, fmt.Errorf("writing chunk: %w", wErr)
			}
			bytesReceived += int64(n)

			// Envia ChunkSACK a cada chunk processado
			if bytesReceived-int64(chunkSeq)*int64(session.ChunkSize) >= int64(session.ChunkSize) {
				if fErr := bufWriter.Flush(); fErr != nil {
					return bytesReceived, fmt.Errorf("flushing before chunk sack: %w", fErr)
				}
				chunkSeq++
				if sErr := protocol.WriteChunkSACK(sackWriter, streamIndex, chunkSeq, uint64(bytesReceived)); sErr != nil {
					logger.Warn("failed to send ChunkSACK", "error", sErr, "stream", streamIndex, "seq", chunkSeq)
				} else {
					logger.Debug("ChunkSACK sent", "stream", streamIndex, "seq", chunkSeq, "offset", bytesReceived)
				}
			}

			// Registra chunk no manifest
			session.Assembler.RegisterChunk(ChunkMeta{
				StreamIndex: streamIndex,
				ChunkSeq:    chunkSeq,
				Offset:      uint64(bytesReceived),
				Length:      int64(n),
			})
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			bufWriter.Flush()
			return bytesReceived, fmt.Errorf("reading from stream %d: %w", streamIndex, readErr)
		}
	}

	if err := bufWriter.Flush(); err != nil {
		return bytesReceived, fmt.Errorf("final flush stream %d: %w", streamIndex, err)
	}

	// Envia ChunkSACK final
	chunkSeq++
	protocol.WriteChunkSACK(sackWriter, streamIndex, chunkSeq, uint64(bytesReceived))

	return bytesReceived, nil
}

// handleParallelJoin processa uma conexão secundária de ParallelJoin.
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
		protocol.WriteParallelACK(conn, protocol.ParallelStatusNotFound)
		return
	}

	pSession, ok := raw.(*ParallelSession)
	if !ok {
		logger.Warn("session is not a parallel session")
		protocol.WriteParallelACK(conn, protocol.ParallelStatusNotFound)
		return
	}

	// Valida stream index
	if pj.StreamIndex >= pSession.MaxStreams {
		logger.Warn("stream index exceeds max", "maxStreams", pSession.MaxStreams)
		protocol.WriteParallelACK(conn, protocol.ParallelStatusFull)
		return
	}

	// ACK OK
	if err := protocol.WriteParallelACK(conn, protocol.ParallelStatusOK); err != nil {
		logger.Error("writing ParallelACK", "error", err)
		return
	}

	// Registra conexão do stream
	pSession.StreamConns.Store(pj.StreamIndex, conn)

	// Abre chunk file para este stream
	chunkFile, _, err := pSession.Assembler.ChunkFile(pj.StreamIndex)
	if err != nil {
		logger.Error("opening chunk file", "error", err)
		return
	}

	// Recebe dados do stream
	bytesReceived, err := h.receiveParallelStream(ctx, conn, conn, chunkFile, pj.StreamIndex, pSession, logger)
	chunkFile.Close()

	if err != nil {
		logger.Error("receiving parallel stream", "error", err, "bytes", bytesReceived)
		return
	}

	logger.Info("parallel stream complete", "bytes", bytesReceived)
}
