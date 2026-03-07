// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_single.go contém toda a lógica de backup single-stream e resume.
//
// O fluxo de single-stream é linear:
//   1. Handshake (agentName, storageName, backupName) já processado por handleBackup
//   2. Recebe dados via receiveWithSACK com ACK periódico
//   3. Valida trailer (SHA-256) e comita atomicamente
//
// O fluxo de resume reconecta a uma sessão parcial existente:
//   1. Busca PartialSession por sessionID
//   2. Reabre o .tmp para append
//   3. Continua receiveWithSACK de onde parou
//
// Inclui também funções utilitárias compartilhadas com handler_parallel.go:
//   - sendACK, readUntilNewline, readTrailerFromFile, hashFile, generateSessionID

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
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// handleBackup processa uma sessão de backup completa.
func (h *Handler) handleBackup(ctx context.Context, conn net.Conn, logger *slog.Logger) {
	// O magic "NBKP" já foi lido; ler restante do handshake (version + agent name + storage name)
	versionBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, versionBuf); err != nil {
		logger.Error("reading protocol version", "error", err)
		return
	}

	handshakeVersion := versionBuf[0]
	if handshakeVersion < 0x03 || handshakeVersion > protocol.ProtocolVersion {
		logger.Error("unsupported protocol version", "version", handshakeVersion)
		protocol.WriteACKLegacy(conn, protocol.StatusReject, "unsupported protocol version", "")
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

	// Emite evento de início de sessão
	if h.Events != nil {
		h.Events.PushEvent("info", "session_start", agentName, fmt.Sprintf("backup %s/%s handshake (v%s)", storageName, backupName, clientVersion), 0)
	}

	// Valida componentes de path contra traversal
	for _, v := range []struct{ val, field string }{
		{agentName, "agentName"},
		{storageName, "storageName"},
		{backupName, "backupName"},
	} {
		if err := validatePathComponent(v.val, v.field); err != nil {
			logger.Warn("invalid path component in handshake", "field", v.field, "value", v.val, "error", err)
			sendACK(conn, handshakeVersion, protocol.StatusReject, fmt.Sprintf("invalid %s: %s", v.field, err), "")
			return
		}
	}

	// Valida identidade: agentName do protocolo deve coincidir com CN do certificado TLS
	certName := h.extractAgentName(conn, logger)
	if certName != "" && certName != agentName {
		logger.Warn("agent identity mismatch: protocol agentName does not match TLS certificate CN",
			"protocol_agent", agentName, "cert_cn", certName)
		sendACK(conn, handshakeVersion, protocol.StatusReject,
			fmt.Sprintf("agent name %q does not match certificate CN %q", agentName, certName), "")
		return
	}

	// Busca storage nomeado
	conn.SetReadDeadline(time.Time{}) // limpa deadline do handshake
	storageInfo, ok := h.cfg.GetStorage(storageName)
	if !ok {
		logger.Warn("storage not found")
		sendACK(conn, handshakeVersion, protocol.StatusStorageNotFound, fmt.Sprintf("storage %q not found", storageName), "")
		return
	}

	// Lock: por agent:storage:backup (permite backups simultâneos de entries diferentes)
	lockKey := agentName + ":" + storageName + ":" + backupName
	if _, loaded := h.locks.LoadOrStore(lockKey, true); loaded {
		logger.Warn("backup already in progress for agent")
		sendACK(conn, handshakeVersion, protocol.StatusBusy, "backup already in progress", "")
		return
	}
	defer h.locks.Delete(lockKey)

	// Gera sessionID
	sessionID := generateSessionID()
	logger = logger.With("session", sessionID)

	// ACK GO — v4+ inclui compression mode, v3 usa legacy
	compressionMode := storageInfo.CompressionModeByte()
	if handshakeVersion >= 0x04 {
		if err := protocol.WriteACK(conn, protocol.StatusGo, "", sessionID, compressionMode); err != nil {
			logger.Error("writing ACK", "error", err)
			return
		}
	} else {
		if err := protocol.WriteACKLegacy(conn, protocol.StatusGo, "", sessionID); err != nil {
			logger.Error("writing ACK", "error", err)
			return
		}
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
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName, backupName, storageInfo.FileExtension())
	if err != nil {
		logger.Error("creating atomic writer", "error", err)
		if ackErr := protocol.WriteParallelInitACK(conn, protocol.ParallelInitStatusError); ackErr != nil {
			logger.Error("writing ParallelInit ACK", "error", ackErr)
		}
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
		TmpPath:         tmpPath,
		AgentName:       agentName,
		StorageName:     storageName,
		BackupName:      backupName,
		BaseDir:         storageInfo.BaseDir,
		CreatedAt:       now,
		ClientVersion:   clientVersion,
		CompressionMode: storageInfo.CompressionMode,
		Phase:           NewSessionPhaseTracker(),
	}
	session.LastActivity.Store(now.UnixNano())
	h.sessions.Store(sessionID, session)
	defer func() {
		// Mantém sessão visível por 3s para que o WebUI capture a fase final
		time.AfterFunc(3*time.Second, func() {
			h.sessions.Delete(sessionID)
		})
	}()

	// Stream com SACK periódico — usa br em vez de conn para não perder dados bufferizados
	bytesReceived, err := h.receiveWithSACK(ctx, br, conn, tmpFile, tmpPath, session, logger)
	tmpFile.Close()

	if err != nil {
		logger.Error("receiving data stream", "error", err, "bytes", bytesReceived)
		// NÃO aborta o tmp — mantém para resume
		return
	}

	// Remove sessão parcial — backup recebido com sucesso, resume não será necessário

	// Validação do trailer e commit
	result, dataSize := h.validateAndCommitSingle(conn, writer, tmpPath, bytesReceived, storageInfo, session, logger)
	h.recordSessionEnd(sessionID, agentName, storageName, backupName, "single", storageInfo.CompressionMode, result, now, dataSize)
	if result == "ok" {
		session.Phase.Set(PhaseDone)
	} else {
		session.Phase.Set(PhaseFailed)
	}
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
	session.BytesWritten.Store(lastOffset)
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
	writer, wErr := NewAtomicWriter(storageInfo.BaseDir, session.AgentName, session.BackupName, storageInfo.FileExtension())
	if wErr != nil {
		logger.Error("creating atomic writer for resume", "error", wErr)
		return
	}

	result, dataSize := h.validateAndCommitSingle(conn, writer, session.TmpPath, totalBytes, storageInfo, nil, logger)
	h.recordSessionEnd(resume.SessionID, session.AgentName, session.StorageName, session.BackupName, "single", session.CompressionMode, result, session.CreatedAt, dataSize)
}

// receiveWithSACK lê dados do conn, escreve no tmpFile, e envia SACKs periódicos.
// Retorna o número de bytes recebidos nesta sessão (não o total do arquivo).
func (h *Handler) receiveWithSACK(ctx context.Context, reader io.Reader, sackWriter io.Writer, tmpFile *os.File, tmpPath string, session *PartialSession, logger *slog.Logger) (int64, error) {
	bufConn := bufio.NewReaderSize(reader, singleStreamIOBufferSize)
	bufFile := bufio.NewWriterSize(tmpFile, singleStreamIOBufferSize)

	var bytesReceived int64
	var lastSACK int64
	var sackErr atomic.Value // armazena erro de SACK para não bloquear

	// Sliding read deadline: reseta a cada read bem-sucedido.
	// Se a rede morrer silenciosamente (sem TCP RST), o read expirará em vez de travar para sempre.
	netConn, hasDeadline := sackWriter.(net.Conn)

	buf := make([]byte, singleStreamIOBufferSize)
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
			totalWritten := session.BytesWritten.Add(int64(n))
			session.LastActivity.Store(time.Now().UnixNano())
			h.TrafficIn.Add(int64(n))
			h.DiskWrite.Add(int64(n))

			// Envia SACK a cada sackInterval bytes
			if bytesReceived-lastSACK >= sackInterval {
				if fErr := bufFile.Flush(); fErr != nil {
					return bytesReceived, fmt.Errorf("flushing before sack: %w", fErr)
				}
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

// validateAndCommitSingle valida o trailer, checksum e comita o backup.
// Retorna (resultado, dataSize). resultado: "ok", "checksum_mismatch" ou "write_error".
// session pode ser nil (resume não tem PartialSession com phase tracker).
func (h *Handler) validateAndCommitSingle(conn net.Conn, writer *AtomicWriter, tmpPath string, totalBytes int64, storageInfo config.StorageInfo, session *PartialSession, logger *slog.Logger) (string, int64) {
	const trailerSize int64 = 4 + 32 + 8

	if totalBytes < trailerSize {
		logger.Error("received data too small", "bytes", totalBytes)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error", 0
	}

	// Lê o trailer dos últimos 44 bytes do arquivo
	trailer, err := readTrailerFromFile(tmpPath, trailerSize)
	if err != nil {
		logger.Error("reading trailer from file", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error", 0
	}

	// Trunca o arquivo para remover o trailer (mantém apenas os dados)
	dataSize := totalBytes - trailerSize
	if err := os.Truncate(tmpPath, dataSize); err != nil {
		logger.Error("truncating temp file", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error", dataSize
	}

	// Calcula SHA-256 dos dados (sem trailer)
	serverChecksum, err := hashFile(tmpPath)
	if err != nil {
		logger.Error("computing server checksum", "error", err)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error", dataSize
	}

	// Compara checksums
	if serverChecksum != trailer.Checksum {
		logger.Error("checksum mismatch",
			"client", fmt.Sprintf("%x", trailer.Checksum),
			"server", fmt.Sprintf("%x", serverChecksum),
		)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusChecksumMismatch)
		return "checksum_mismatch", dataSize
	}

	// Commit (rename atômico)
	finalPath, err := writer.Commit(tmpPath)
	if err != nil {
		logger.Error("committing backup", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error", dataSize
	}

	// Verifica integridade do archive antes de rotacionar.
	// Se falhar, o backup fica no disco mas NÃO apaga os antigos (fail-safe).
	if storageInfo.VerifyIntegrity {
		var intProgress *IntegrityProgress
		if session != nil {
			session.Phase.Set(PhaseVerifying)
			intProgress = NewIntegrityProgress(0)
			session.IntProgress = intProgress
		}
		logger.Info("verifying backup integrity", "path", finalPath)
		if vErr := VerifyArchiveIntegrity(finalPath, intProgress, logger); vErr != nil {
			logger.Error("backup integrity check failed — skipping rotation",
				"path", finalPath, "error", vErr)
			if h.Events != nil {
				h.Events.PushEvent("error", "integrity_failed", writer.AgentName(),
					fmt.Sprintf("integrity check failed for %s: %v", finalPath, vErr), 0)
			}
			protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
			return "ok", dataSize
		}
		logger.Info("backup integrity verified", "path", finalPath)
	}

	// Archive pre-Rotate: envia backups que SERÃO deletados pelo Rotate
	// (antes da deleção, para que os arquivos ainda existam no disco).
	if hasArchiveBuckets(storageInfo.Buckets) {
		candidates, _ := ListRotationCandidates(writer.AgentDir(), storageInfo.MaxBackups)
		h.runArchivePreRotate(storageInfo, candidates, writer.AgentDir(), logger)
	}

	// Rotação
	removed, err := Rotate(writer.AgentDir(), storageInfo.MaxBackups)
	if err != nil {
		logger.Warn("rotation failed", "error", err)
	}
	for _, name := range removed {
		logger.Info("backup rotated (deleted)", "file", name)
		if h.Events != nil {
			h.Events.PushEvent("warn", "backup_rotated", writer.AgentName(), fmt.Sprintf("deleted old backup: %s", name), 0)
		}
	}

	// Object Storage pós-commit (sync/offload — archive já tratado acima)
	// Offload bloqueia até upload confirmado; sync é fire-and-forget.
	if session != nil && len(filterBucketsExcluding(storageInfo.Buckets, config.BucketModeArchive)) > 0 {
		session.Phase.Set(PhaseUploading)
		session.PCProgress = NewPostCommitProgress()
	}
	h.runPostCommitSync(storageInfo, finalPath, removed, writer.AgentDir(), logger)

	logger.Info("backup committed",
		"path", finalPath,
		"bytes", dataSize,
		"checksum", fmt.Sprintf("%x", serverChecksum),
	)

	protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
	return "ok", dataSize
}

// maxHandshakeFieldLen é o comprimento máximo permitido para campos do handshake
// (agentName, storageName, backupName, clientVersion).
const maxHandshakeFieldLen = 512

// sendACK envia um ACK condicional baseado na versão do handshake.
// Para v4+, inclui o byte de CompressionMode (default gzip para rejeições).
// Para v3, usa WriteACKLegacy sem o byte adicional.
func sendACK(conn net.Conn, handshakeVersion byte, status byte, message, sessionID string) error {
	if handshakeVersion >= 0x04 {
		return protocol.WriteACK(conn, status, message, sessionID, protocol.CompressionGzip)
	}
	return protocol.WriteACKLegacy(conn, status, message, sessionID)
}

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
