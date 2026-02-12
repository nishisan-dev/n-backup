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

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// Handler processa conexões individuais de backup.
type Handler struct {
	cfg    *config.ServerConfig
	logger *slog.Logger
	locks  *sync.Map // Mapa de locks por "agent:storage"
}

// NewHandler cria um novo Handler.
func NewHandler(cfg *config.ServerConfig, logger *slog.Logger, locks *sync.Map) *Handler {
	return &Handler{
		cfg:    cfg,
		logger: logger,
		locks:  locks,
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

	// Stream: conn → buffer → arquivo .tmp (256KB buffers para reduzir syscalls)
	bufConn := bufio.NewReaderSize(conn, 256*1024)
	bufFile := bufio.NewWriterSize(tmpFile, 256*1024)

	bytesReceived, err := io.Copy(bufFile, bufConn)

	// Flush antes de fechar
	if flushErr := bufFile.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	tmpFile.Close()

	if err != nil {
		logger.Error("receiving data stream", "error", err)
		writer.Abort(tmpPath)
		return
	}

	logger.Info("data stream received", "bytes", bytesReceived)

	const trailerSize int64 = 4 + 32 + 8

	if bytesReceived < trailerSize {
		logger.Error("received data too small", "bytes", bytesReceived)
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
	dataSize := bytesReceived - trailerSize
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
