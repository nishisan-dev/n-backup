// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_parallel.go contém toda a lógica de backup paralelo:
//
//   - ParallelSession (struct + abort/aborted helpers)
//   - handleParallelBackup — orquestração da sessão paralela: criação do
//     assembler, espera por streams, ingestion done, finalize e commit
//   - receiveParallelStream — recebe chunks com ChunkHeader framing por stream
//   - readParallelChunkPayload — lê payload de um chunk individual
//   - handleParallelJoin — processa conexões secundárias (join/re-join)
//   - validateAndCommitWithTrailer — validação e commit para backup paralelo
//
// O fluxo paralelo funciona assim:
//   1. Agent envia handshake + ParallelInit (MaxStreams, ChunkSize)
//   2. Server cria ParallelSession com slots pré-alocados
//   3. Agent abre N conexões secundárias via ParallelJoin (PJIN)
//   4. Cada stream envia chunks com ChunkHeader (GlobalSeq + Length + SlotID)
//   5. Server monta arquivo via ChunkAssembler (in-order ou buffered)
//   6. Agent envia ControlIngestionDone quando toda ingestão terminou
//   7. Server finaliza assembly, recebe Trailer, valida SHA-256 e comita

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/logging"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// ParallelSession rastreia uma sessão de backup com streams paralelos.
type ParallelSession struct {
	SessionID       string
	Assembler       *ChunkAssembler
	Writer          *AtomicWriter
	StorageInfo     config.StorageInfo
	AgentName       string
	StorageName     string
	BackupName      string
	Slots           []*Slot // pré-alocados no ParallelInitACK, indexados por SlotID
	MaxStreams      uint8
	ChunkSize       uint32
	StreamWg        sync.WaitGroup // barreira para todos os streams
	Closing         atomic.Bool    // true após StreamWg.Wait() retornar — rejeita novos Add()
	StreamReady     chan struct{}  // fechado quando o primeiro stream conecta
	streamReadyOnce sync.Once      // garante close único do StreamReady
	Done            chan struct{}  // sinaliza conclusão
	CreatedAt       time.Time
	LastActivity    atomic.Int64  // UnixNano do último I/O bem-sucedido
	DiskWriteBytes  atomic.Int64  // Total de bytes escritos em disco nesta sessão
	TotalObjects    atomic.Uint32 // Total de objetos a enviar (recebido via ControlProgress)
	ObjectsSent     atomic.Uint32 // Objetos já enviados (recebido via ControlProgress)
	WalkComplete    atomic.Int32  // 1 = prescan concluído, total confiável (via ControlProgress)
	ClientVersion   string        // Versão do client (protocolo v3+)
	AutoScaleInfo   atomic.Value  // *observability.AutoScaleInfo (atualizado via ControlAutoScaleStats)
	IngestionDone    chan struct{} // fechado quando agent envia ControlIngestionDone
	ingestionOnce    sync.Once     // garante close único do IngestionDone
	Aborted          chan struct{} // fechado quando a sessão é abortada antes do finalize
	abortOnce        sync.Once     // garante close único do Aborted
	AbortErr         atomic.Value  // error do aborto (quando houver)
	ControlLost      chan struct{} // fechado quando o control channel deste agent cai
	controlLostMu    sync.Mutex    // protege ControlLost + controlLostOnce para reset thread-safe
	controlLostOnce  sync.Once     // garante close único do ControlLost

	// Lifecycle phases — rastreamento de fase pós-streaming para WebUI
	Phase      *SessionPhaseTracker // fase atual da sessão
	IntProgress *IntegrityProgress   // progresso da verificação de integridade (nil quando não ativo)
	PCProgress  *PostCommitProgress  // progresso do upload pós-commit (nil quando não ativo)

	Logger *slog.Logger // Session logger (enriquecido com session_log_dir quando habilitado)
}

// signalControlLost fecha o channel ControlLost de forma segura (idempotente).
// Chamado por handleControlChannel quando o control channel do agent é encerrado.
func (ps *ParallelSession) signalControlLost() {
	ps.controlLostMu.Lock()
	defer ps.controlLostMu.Unlock()
	ps.controlLostOnce.Do(func() { close(ps.ControlLost) })
}

// resetControlLost recria o channel ControlLost, permitindo que novas quedas
// do control channel sejam detectadas. Chamado quando o agent reconecta.
// Thread-safe: protegido pelo mesmo mutex de signalControlLost.
func (ps *ParallelSession) resetControlLost() {
	ps.controlLostMu.Lock()
	defer ps.controlLostMu.Unlock()
	ps.ControlLost = make(chan struct{})
	ps.controlLostOnce = sync.Once{}
}

// abort marca a sessão como abortada, fecha o channel Aborted e cancela todos os slots.
func (ps *ParallelSession) abort(err error) {
	if err != nil {
		ps.AbortErr.Store(err)
	}
	ps.Closing.Store(true)
	ps.abortOnce.Do(func() {
		close(ps.Aborted)
	})

	for _, slot := range ps.Slots {
		if slot.CancelFn != nil {
			slot.CancelFn()
		}
		slot.ConnMu.Lock()
		if slot.Conn != nil {
			slot.Conn.Close()
		}
		slot.ConnMu.Unlock()
	}
}

// aborted verifica se a sessão foi abortada e retorna o erro associado.
func (ps *ParallelSession) aborted() (error, bool) {
	select {
	case <-ps.Aborted:
		if err, ok := ps.AbortErr.Load().(error); ok {
			return err, true
		}
		return nil, true
	default:
		return nil, false
	}
}

// handleParallelBackup processa um backup paralelo.
// A conexão primária é usada apenas como canal de controle (Trailer + FinalACK).
// Todos os dados são recebidos via streams secundários (ParallelJoin).
func (h *Handler) handleParallelBackup(ctx context.Context, conn net.Conn, br io.Reader, sessionID, agentName, storageName, backupName, clientVersion string, storageInfo config.StorageInfo, pi *protocol.ParallelInit, lockKey string, logger *slog.Logger) {
	defer h.locks.Delete(lockKey)

	logger = logger.With("session", sessionID, "mode", "parallel", "maxStreams", pi.MaxStreams)

	// Session logger: grava logs desta sessão em arquivo dedicado para post-mortem.
	var sessionLogPath string
	if h.cfg.Logging.SessionLogDir != "" {
		var sessionLogCloser io.Closer
		var slErr error
		logger, sessionLogCloser, sessionLogPath, slErr = logging.NewSessionLogger(
			logger, h.cfg.Logging.SessionLogDir, agentName, sessionID)
		if slErr != nil {
			logger.Warn("failed to create session logger", "error", slErr)
		} else {
			defer sessionLogCloser.Close()
			logger.Info("session log file created", "path", sessionLogPath)
		}
	}

	logger.Info("starting parallel backup session")

	// Emite evento de início de sessão paralela
	if h.Events != nil {
		h.Events.PushEvent("info", "session_start", agentName, fmt.Sprintf("parallel backup %s/%s (%d streams)", storageName, backupName, pi.MaxStreams), 0)
	}

	// Prepara escrita atômica
	writer, err := NewAtomicWriter(storageInfo.BaseDir, agentName, backupName, storageInfo.FileExtension())
	if err != nil {
		logger.Error("creating atomic writer", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Cria assembler para staging de chunks (configurável por storage)
	assembler, err := NewChunkAssemblerWithOptions(sessionID, writer.AgentDir(), logger, ChunkAssemblerOptions{
		Mode:             storageInfo.AssemblerMode,
		PendingMemLimit:  storageInfo.AssemblerPendingMemRaw,
		ShardLevels:      storageInfo.ChunkShardLevels,
		FsyncChunkWrites: storageInfo.FsyncChunkWrites(),
	})
	if err != nil {
		logger.Error("creating chunk assembler", "error", err)
		if ackErr := protocol.WriteParallelInitACK(conn, protocol.ParallelInitStatusError); ackErr != nil {
			logger.Error("writing ParallelInit ACK", "error", ackErr)
		}
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
		Slots:         PreallocateSlots(pi.MaxStreams),
		MaxStreams:    pi.MaxStreams,
		ChunkSize:     pi.ChunkSize,
		StreamReady:   make(chan struct{}),
		Done:          make(chan struct{}),
		CreatedAt:     now,
		IngestionDone: make(chan struct{}),
		Aborted:       make(chan struct{}),
		ControlLost:   make(chan struct{}),
		Phase:         NewSessionPhaseTracker(),
	}

	pSession.Logger = logger // session logger (com fan-out para arquivo quando habilitado)
	pSession.LastActivity.Store(now.UnixNano())
	h.sessions.Store(sessionID, pSession)

	if err := protocol.WriteParallelInitACK(conn, protocol.ParallelInitStatusOK); err != nil {
		logger.Error("writing ParallelInit ACK", "error", err)
		h.sessions.Delete(sessionID)
		return
	}

	// Conn primária é control-only: não recebe dados de stream 0 aqui.
	// Todos os N streams de dados conectam via ParallelJoin (handleParallelJoin).

	// Espera pelo menos 1 stream conectar (via canal StreamReady),
	// depois espera todos terminarem via StreamWg.
	select {
	case <-pSession.StreamReady:
		// Pelo menos 1 stream conectou — espera todos finalizarem.
	case <-ctx.Done():
		logger.Error("context cancelled waiting for streams")
		h.sessions.Delete(sessionID)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	case <-time.After(5 * time.Minute):
		logger.Error("timeout waiting for streams to connect")
		h.sessions.Delete(sessionID)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	// Espera sinal explícito do agent (ControlIngestionDone) ou timeout.
	// StreamWg.Wait() só garante cleanup das goroutines após o sinal.
	select {
	case <-pSession.IngestionDone:
		logger.Info("agent confirmed ingestion complete")
	case <-pSession.ControlLost:
		// Control channel caiu — aguarda reconexão por grace period antes de abortar.
		gracePeriod := h.cfg.ControlLostGracePeriod
		logger.Warn("control channel lost during active session, waiting for reconnection",
			"grace_period", gracePeriod)
		select {
		case <-pSession.IngestionDone:
			logger.Info("agent delivered IngestionDone after control reconnect")
		case <-time.After(gracePeriod):
			logger.Error("control channel not recovered after grace period — aborting session",
				"grace_period", gracePeriod)
			pSession.abort(fmt.Errorf("control channel lost and not recovered within %s", gracePeriod))
			h.recordSessionEnd(sessionID, agentName, storageName, backupName, "parallel",
				storageInfo.CompressionMode, "control_lost", now, pSession.DiskWriteBytes.Load())
			h.sessions.Delete(sessionID)
			protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
			if h.Events != nil {
				h.Events.PushEvent("error", "session_control_lost", agentName,
					fmt.Sprintf("%s/%s aborted: control channel lost for %s", storageName, backupName, gracePeriod), 0)
			}
			return
		case <-ctx.Done():
			logger.Error("context cancelled during control lost grace period")
			h.sessions.Delete(sessionID)
			protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
			return
		}
	case <-pSession.Aborted:
		pSession.StreamWg.Wait()
		// Sinaliza ao ChunkBuffer para parar de drenar chunks desta sessão,
		// evitando cascade de I/O errors em diretórios já removidos.
		if h.chunkBuffer != nil {
			h.chunkBuffer.MarkSessionAborted(assembler)
		}
		err, _ := pSession.aborted()
		if err != nil {
			logger.Error("parallel session aborted before ingestion completed", "error", err)
		} else {
			logger.Error("parallel session aborted before ingestion completed")
		}
		h.sessions.Delete(sessionID)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		if h.Events != nil {
			msg := fmt.Sprintf("%s/%s aborted before ingestion completed", storageName, backupName)
			if err != nil {
				msg = fmt.Sprintf("%s/%s aborted before ingestion completed: %v", storageName, backupName, err)
			}
			h.Events.PushEvent("error", "session_aborted", agentName, msg, 0)
		}
		return
	case <-time.After(25 * time.Hour):
		logger.Error("ingestion timeout — agent never confirmed completion")
		h.sessions.Delete(sessionID)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		if h.Events != nil {
			h.Events.PushEvent("error", "ingestion_timeout", agentName, fmt.Sprintf("%s/%s timed out waiting for ControlIngestionDone", storageName, backupName), 0)
		}
		return
	case <-ctx.Done():
		logger.Error("context cancelled waiting for ingestion done")
		h.sessions.Delete(sessionID)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return
	}

	pSession.Closing.Store(true)
	pSession.Phase.Set(PhaseAssembling)
	pSession.StreamWg.Wait()
	logger.Info("all parallel streams complete — proceeding to finalize")

	// Evento de ingestão completa — a sessão permanece visível com status "finalizing"
	if h.Events != nil {
		h.Events.PushEvent("info", "ingestion_complete", agentName, fmt.Sprintf("%s/%s ingestion finished, assembling file", storageName, backupName), 0)
	}

	// Primeiro flush do buffer de memória: garante que nenhum chunk ainda em
	// trânsito no ChunkBuffer seja perdido antes do Finalize.
	// FIX: Flush é scoped por sessão — não bloqueia sessões concorrentes.
	if h.chunkBuffer != nil {
		if err := h.chunkBuffer.Flush(assembler); err != nil {
			logger.Error("flushing chunk buffer before finalize", "error", err)
			h.recordSessionEnd(sessionID, agentName, storageName, backupName, "parallel",
				storageInfo.CompressionMode, "flush_timeout", now, pSession.DiskWriteBytes.Load())
			h.sessions.Delete(sessionID)
			protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
			if h.Events != nil {
				h.Events.PushEvent("error", "flush_timeout", agentName,
					fmt.Sprintf("%s/%s flush failed: %v", storageName, backupName, err), 0)
			}
			return
		}
		// Verifica se algum chunk falhou permanentemente durante a drenagem.
		// Sem esta verificação, finalizeLazy() retornaria "missing chunk seq N"
		// sem contexto do que realmente ocorreu (falha de I/O no drainSlot).
		if err := h.chunkBuffer.SessionFailed(assembler); err != nil {
			logger.Error("session aborted: chunk buffer drain failure", "error", err)
			protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
			if h.Events != nil {
				h.Events.PushEvent("error", "drain_failure", agentName,
					fmt.Sprintf("%s/%s chunk drain failed: %v", storageName, backupName, err), 0)
			}
			return
		}
	}
	// Log do estado do assembler antes do Finalize para diagnóstico post-mortem.
	preStats := assembler.Stats()
	logger.Info("pre_finalize_state",
		"nextExpectedSeq", preStats.NextExpectedSeq,
		"pendingChunks", preStats.PendingChunks,
		"pendingMemBytes", preStats.PendingMemBytes,
		"totalBytes", preStats.TotalBytes,
		"totalChunks", preStats.TotalChunks,
		"phase", preStats.Phase,
	)

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
	result := h.validateAndCommitWithTrailer(conn, writer, assembledPath, totalBytes, trailer, serverChecksum, storageInfo, pSession, lockKey, logger)
	h.recordSessionEnd(sessionID, agentName, storageName, backupName, "parallel", storageInfo.CompressionMode, result, now, totalBytes)
	if result == "ok" {
		pSession.Phase.Set(PhaseDone)
	} else {
		pSession.Phase.Set(PhaseFailed)
	}
	// Mantém sessão visível por 3s para que o WebUI capture a fase final
	time.AfterFunc(3*time.Second, func() {
		h.sessions.Delete(sessionID)
	})

	// Gerencia arquivo de log da sessão: remove em sucesso, retém para post-mortem em falha.
	if sessionLogPath != "" {
		if result == "ok" {
			logging.RemoveSessionLog(h.cfg.Logging.SessionLogDir, agentName, sessionID)
			logger.Info("session log removed (backup ok)")
		} else {
			logger.Warn("session log retained for post-mortem", "path", sessionLogPath, "result", result)
		}
	}
}

// readParallelChunkPayload lê o payload de um chunk paralelo.
// O deadline TCP usa streamReadDeadline (mesma constante usada para o header).
func (h *Handler) readParallelChunkPayload(conn net.Conn, reader io.Reader, length uint32, globalSeq uint32, session *ParallelSession) ([]byte, error) {
	buf := make([]byte, length)

	for offset := 0; offset < len(buf); {
		conn.SetReadDeadline(time.Now().Add(streamReadDeadline))
		n, err := reader.Read(buf[offset:])
		if n > 0 {
			offset += n

		}
		if err != nil {
			return nil, fmt.Errorf("reading chunk seq %d payload: %w", globalSeq, err)
		}
	}

	return buf, nil
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

	// Recupera offset corrente do slot para suporte a resume
	slot := session.Slots[streamIndex]
	bytesReceived = slot.Offset.Load()
	if bytesReceived > 0 {
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

		// Lê ChunkHeader (13 bytes: GlobalSeq + Length + SlotID + CRC32)
		hdr, err := protocol.ReadChunkHeader(reader)
		if err != nil {
			if err == io.EOF || err.Error() == "reading chunk header seq: EOF" {
				break
			}
			return bytesReceived, fmt.Errorf("reading chunk header from stream %d: %w", streamIndex, err)
		}

		// Marco de início do chunk: o header foi lido com sucesso e o server vai
		// iniciar a leitura do payload. Isso ajuda a distinguir "nunca chegou" de
		// "chegou o header, mas falhou/travou durante o payload".
		logger.Debug("chunk_receive_started",
			"stream", streamIndex,
			"globalSeq", hdr.GlobalSeq,
			"length", hdr.Length,
			"offsetBefore", bytesReceived,
		)

		chunkData, err := h.readParallelChunkPayload(conn, reader, hdr.Length, hdr.GlobalSeq, session)
		if err != nil {
			return bytesReceived, err
		}

		// Validação de integridade per-chunk via CRC32 IEEE (Protocol v6).
		// Rejeita chunk se mismatch — força reconexão do stream.
		computedCRC := crc32.ChecksumIEEE(chunkData)
		if computedCRC != hdr.CRC32 {
			logger.Error("chunk_crc_mismatch",
				"stream", streamIndex,
				"globalSeq", hdr.GlobalSeq,
				"expected_crc", fmt.Sprintf("%08x", hdr.CRC32),
				"computed_crc", fmt.Sprintf("%08x", computedCRC),
				"length", hdr.Length,
			)
			if h.Events != nil {
				h.Events.PushEvent("error", "chunk_crc_mismatch", session.AgentName,
					fmt.Sprintf("stream %d seq %d: CRC32 %08x != %08x",
						streamIndex, hdr.GlobalSeq, computedCRC, hdr.CRC32), 0)
			}
			return bytesReceived, fmt.Errorf("%w: stream %d seq %d expected %08x got %08x",
				protocol.ErrChunkCRCMismatch, streamIndex, hdr.GlobalSeq, hdr.CRC32, computedCRC)
		}

		// Entrega o chunk ao assembler — diretamente ou via buffer de memória.
		// Quando o buffer está habilitado, Push materializa os dados do reader TCP
		// em memória e retorna imediatamente; o drainer fará a escrita de forma
		// assíncrona, desacoplando a goroutine de rede do I/O de disco.
		if h.chunkBuffer != nil {
			buffered, err := h.chunkBuffer.Push(hdr.GlobalSeq, chunkData, session.Assembler, nil)
			if err != nil {
				logger.Warn("chunk_receive_failed",
					"stream", streamIndex,
					"globalSeq", hdr.GlobalSeq,
					"error", err,
				)
				// Backpressure: buffer cheio após timeout — falha a stream para forçar
				// reconexão do agent e aliviar pressão.
				return bytesReceived, fmt.Errorf("chunk buffer push on seq %d: %w", hdr.GlobalSeq, err)
			}
			if buffered {
				logger.Debug("chunk_buffered",
					"stream", streamIndex,
					"globalSeq", hdr.GlobalSeq,
				)
			}
		} else {
			// Caminho direto (buffer desabilitado) — payload já foi materializado acima.
			if err := session.Assembler.WriteChunk(hdr.GlobalSeq, bytes.NewReader(chunkData), int64(hdr.Length)); err != nil {
				logger.Warn("chunk_receive_failed",
					"stream", streamIndex,
					"globalSeq", hdr.GlobalSeq,
					"error", err,
				)
				return bytesReceived, fmt.Errorf("writing chunk seq %d to assembler: %w", hdr.GlobalSeq, err)
			}
		}

		nowNano := time.Now().UnixNano()
		bytesReceived += int64(hdr.Length) + protocol.ChunkHeaderSize
		session.LastActivity.Store(nowNano)
		h.TrafficIn.Add(int64(hdr.Length))
		h.DiskWrite.Add(int64(hdr.Length))
		session.DiskWriteBytes.Add(int64(hdr.Length))

		// Log detalhado de chunk recebido — vai para o arquivo de sessão (DEBUG)
		// e para stdout apenas se o nível global for DEBUG.
		logger.Debug("chunk_received",
			"stream", streamIndex,
			"globalSeq", hdr.GlobalSeq,
			"length", hdr.Length,
			"totalBytes", bytesReceived,
		)

		// Per-slot stats: incrementa tráfego e atualiza last activity
		slot.TrafficIn.Add(int64(hdr.Length))
		slot.LastActivity.Store(nowNano)
		slot.ChunksReceived.Add(1)
		slot.LastChunkSeq.Store(hdr.GlobalSeq)

		// Atualiza offset atômico — usado por handleParallelJoin para resume
		slot.Offset.Store(bytesReceived)

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

	// A partir daqui já temos uma sessão válida: quando disponível, redireciona
	// os logs do join para o session logger para facilitar correlação post-mortem.
	if pSession.Logger != nil {
		logger = pSession.Logger.With("stream", pj.StreamIndex, "remote", conn.RemoteAddr().String())
	} else {
		logger = logger.With("stream", pj.StreamIndex, "remote", conn.RemoteAddr().String())
	}

	// Rejeita join se a sessão está em fase de fechamento.
	// O check precisa acontecer antes de cancelar/substituir o stream existente
	// e antes de responder ACK OK.
	if pSession.Closing.Load() {
		logger.Warn("session closing, rejecting late join", "stream", pj.StreamIndex)
		protocol.WriteParallelACK(conn, protocol.ParallelStatusNotFound, 0)
		return
	}

	// --- Cancelamento da goroutine anterior (proteção contra goroutine leak) ---
	// Se este stream já foi conectado antes (re-join), cancela o contexto da goroutine
	// anterior para que ela saia imediatamente em vez de esperar o read timeout.
	slot := pSession.Slots[pj.StreamIndex]
	if oldCancel := slot.CancelFn; oldCancel != nil {
		logger.Info("cancelling previous stream goroutine for re-join", "stream", pj.StreamIndex)
		oldCancel()
	}

	// Fecha a conexão anterior para desbloquear reads pendentes
	slot.ConnMu.Lock()
	if slot.Conn != nil {
		slot.Conn.Close()
	}
	slot.ConnMu.Unlock()

	// Verifica se é re-join (stream já conectou antes) — resume offset
	var lastOffset uint64
	if currentOffset := slot.Offset.Load(); currentOffset > 0 {
		lastOffset = uint64(currentOffset)
		logger.Info("parallel stream re-join (resume)", "lastOffset", lastOffset)
	}

	// ACK OK com lastOffset para negociação de resume
	if err := protocol.WriteParallelACK(conn, protocol.ParallelStatusOK, lastOffset); err != nil {
		logger.Error("writing ParallelACK", "error", err)
		return
	}

	// Registra conexão do slot
	slot.ConnMu.Lock()
	slot.Conn = conn
	slot.ConnMu.Unlock()
	slot.SetStatus(SlotReceiving)

	// Atualiza uptime e reconnects/rotations do slot
	var reconnectCount int32
	if slot.GetConnectedAt().IsZero() {
		// Primeira conexão
		reconnectCount = 0
	} else if pj.Flags == protocol.JoinReasonRotation {
		// Port rotation intencional — não conta como reconnect
		rotationCount := slot.Rotations.Add(1)
		if h.Events != nil {
			h.Events.PushEvent("info", "port_rotation", pSession.AgentName, fmt.Sprintf("stream %d port rotation (session %s, rotation #%d)", pj.StreamIndex, pj.SessionID, rotationCount), int(pj.StreamIndex))
		}
	} else {
		// Re-join por erro de rede
		reconnectCount = slot.Reconnects.Add(1)
		if h.Events != nil {
			h.Events.PushEvent("warn", "stream_reconnect", pSession.AgentName, fmt.Sprintf("stream %d re-joined (session %s)", pj.StreamIndex, pj.SessionID), int(pj.StreamIndex))
		}
	}
	slot.SetConnectedAt(time.Now())
	logger.Info("parallel join accepted", "lastOffset", lastOffset, "reconnects", reconnectCount)

	// Atualiza last activity do slot
	slot.LastActivity.Store(time.Now().UnixNano())

	// Cria contexto cancelável para esta goroutine específica.
	// Será cancelado se outro re-join chegar para o mesmo stream index.
	streamCtx, streamCancel := context.WithCancel(ctx)
	slot.CancelFn = streamCancel

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
	// Usa o session logger da ParallelSession (com fan-out para arquivo de sessão)
	// em vez do logger da conexão TCP do stream.
	streamLogger := pSession.Logger.With("stream", pj.StreamIndex, "remote", conn.RemoteAddr().String())
	bytesReceived, err := h.receiveParallelStream(streamCtx, conn, conn, conn, pj.StreamIndex, pSession, streamLogger)
	pSession.StreamWg.Done()

	if err != nil {
		if abortErr, aborted := pSession.aborted(); aborted {
			if abortErr != nil {
				logger.Info("parallel stream closed due to session abort", "bytes", bytesReceived, "error", abortErr)
			} else {
				logger.Info("parallel stream closed due to session abort", "bytes", bytesReceived)
			}
			slot.SetStatus(SlotDisconnected)
			return
		}
		// context.Canceled é esperado em re-join — não é um erro real
		if ctx.Err() == nil && streamCtx.Err() == context.Canceled {
			logger.Info("parallel stream replaced by re-join", "bytes", bytesReceived)
			return
		}
		if errors.Is(err, net.ErrClosed) || (ctx.Err() != nil && streamCtx.Err() == context.Canceled) {
			logger.Info("parallel stream closed", "bytes", bytesReceived, "error", err)
			slot.SetStatus(SlotDisconnected)
			return
		}
		logger.Error("receiving parallel stream", "error", err, "bytes", bytesReceived)
		slot.SetStatus(SlotDisconnected)
		if h.Events != nil {
			h.Events.PushEvent("warn", "stream_disconnected", pSession.AgentName, fmt.Sprintf("stream %d disconnected with error: %v", pj.StreamIndex, err), int(pj.StreamIndex))
		}
		return
	}

	slot.SetStatus(SlotDisconnected)
	if h.Events != nil {
		h.Events.PushEvent("info", "stream_disconnected", pSession.AgentName, fmt.Sprintf("stream %d disconnected (normal, %s received)", pj.StreamIndex, formatBytesGo(bytesReceived)), int(pj.StreamIndex))
	}
	logger.Info("parallel stream complete", "bytes", bytesReceived)
}

// lockKey identifica o lock agent:storage:backup para liberação antecipada em async_upload.
func (h *Handler) validateAndCommitWithTrailer(conn net.Conn, writer *AtomicWriter, tmpPath string, totalBytes int64, trailer *protocol.Trailer, serverChecksum [32]byte, storageInfo config.StorageInfo, pSession *ParallelSession, lockKey string, logger *slog.Logger) string {
	if totalBytes == 0 {
		logger.Error("no data received")
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error"
	}

	// Compara checksums
	if serverChecksum != trailer.Checksum {
		logger.Error("checksum mismatch",
			"client", fmt.Sprintf("%x", trailer.Checksum),
			"server", fmt.Sprintf("%x", serverChecksum),
		)

		// P1-5: Hash parcial para diagnóstico — re-lê o arquivo em checkpoints
		// para identificar onde a divergência de dados começa.
		checkpoints := []int64{1 << 20, 10 << 20, 100 << 20} // 1MB, 10MB, 100MB
		if f, err := os.Open(tmpPath); err == nil {
			h := sha256.New()
			buf := make([]byte, 32*1024)
			var read int64
			cpIdx := 0
			for cpIdx < len(checkpoints) {
				n, readErr := f.Read(buf)
				if n > 0 {
					h.Write(buf[:n])
					read += int64(n)
				}
				if cpIdx < len(checkpoints) && read >= checkpoints[cpIdx] {
					logger.Error("partial_hash_checkpoint",
						"offset", checkpoints[cpIdx],
						"partial_sha256", fmt.Sprintf("%x", h.Sum(nil)),
					)
					cpIdx++
				}
				if readErr != nil {
					break
				}
			}
			f.Close()
		}

		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusChecksumMismatch)
		return "checksum_mismatch"
	}

	// Verifica tamanho
	if uint64(totalBytes) != trailer.Size {
		logger.Error("size mismatch",
			"client", trailer.Size,
			"server", totalBytes,
		)
		writer.Abort(tmpPath)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error"
	}

	// Commit (rename atômico)
	finalPath, err := writer.Commit(tmpPath)
	if err != nil {
		logger.Error("committing backup", "error", err)
		protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
		return "write_error"
	}

	// Verifica integridade do archive antes de rotacionar.
	// Se falhar, o backup fica no disco mas NÃO apaga os antigos (fail-safe).
	if storageInfo.VerifyIntegrity {
		pSession.Phase.Set(PhaseVerifying)
		pSession.IntProgress = NewIntegrityProgress(0) // TotalBytes será setado por VerifyArchiveIntegrity
		logger.Info("verifying backup integrity", "path", finalPath)
		if vErr := VerifyArchiveIntegrity(finalPath, pSession.IntProgress, logger); vErr != nil {
			logger.Error("backup integrity check failed — skipping rotation",
				"path", finalPath, "error", vErr)
			if h.Events != nil {
				h.Events.PushEvent("error", "integrity_failed", writer.AgentName(),
					fmt.Sprintf("integrity check failed for %s: %v", finalPath, vErr), 0)
			}
			protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
			return "ok"
		}
		logger.Info("backup integrity verified", "path", finalPath)
	}

	// Archive pre-Rotate: envia backups que SERÃO deletados pelo Rotate
	// (antes da deleção, para que os arquivos ainda existam no disco).
	if hasArchiveBuckets(storageInfo.Buckets) {
		candidates, _ := ListRotationCandidates(writer.AgentDir(), storageInfo.MaxBackups)
		h.runArchivePreRotate(storageInfo, candidates, writer.AgentDir(), BucketUploadContext{Agent: pSession.AgentName, Storage: pSession.StorageName, Backup: pSession.BackupName, SessionID: pSession.SessionID}, logger)
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
	if len(filterBucketsExcluding(storageInfo.Buckets, config.BucketModeArchive)) > 0 {
		pSession.Phase.Set(PhaseUploading)
		pSession.PCProgress = NewPostCommitProgress()
	}

	// async_upload: libera lock e envia FinalACK ANTES do sync para permitir novas sessões.
	// Offload nunca chega aqui com async_upload=true (validação de config impede).
	if shouldAsyncUpload(storageInfo.Buckets) {
		logger.Info("backup committed (async upload)",
			"path", finalPath,
			"bytes", totalBytes,
			"checksum", fmt.Sprintf("%x", serverChecksum),
		)
		protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
		// Libera lock explicitamente — o defer é idempotente (sync.Map.Delete noop)
		h.locks.Delete(lockKey)
		go h.runPostCommitSync(storageInfo, finalPath, removed, writer.AgentDir(), BucketUploadContext{Agent: pSession.AgentName, Storage: pSession.StorageName, Backup: pSession.BackupName, SessionID: pSession.SessionID}, logger)
		return "ok"
	}

	h.runPostCommitSync(storageInfo, finalPath, removed, writer.AgentDir(), BucketUploadContext{Agent: pSession.AgentName, Storage: pSession.StorageName, Backup: pSession.BackupName, SessionID: pSession.SessionID}, logger)

	logger.Info("backup committed",
		"path", finalPath,
		"bytes", totalBytes,
		"checksum", fmt.Sprintf("%x", serverChecksum),
	)

	protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
	return "ok"
}
