// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// maxResumeAttempts é o número máximo de tentativas de resume antes de reiniciar.
const maxResumeAttempts = 5

// resumeBackoff é o tempo inicial entre tentativas de resume.
const resumeBackoff = 2 * time.Second

// MaxBackupDuration define o tempo máximo que um backup pode rodar antes de ser cancelado.
const MaxBackupDuration = 24 * time.Hour

// RunBackup executa uma sessão completa de backup com suporte a resume.
//
// Pipeline:
//
//	Scanner → tar.gz → RingBuffer (produtor)
//	RingBuffer → conn (sender)
//	conn → SACKs → RingBuffer.Advance (ACK reader)
//
// Se a conexão cair, o sender reconecta, envia RESUME,
// e continua de onde parou (se o offset ainda estiver no buffer).
func RunBackup(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger, progress *ProgressReporter, job *BackupJob, controlCh *ControlChannel) error {
	logger = logger.With("backup", entry.Name, "storage", entry.Storage)
	logger.Info("starting backup session", "server", cfg.Server.Address)

	// Configura TLS
	tlsCfg, err := pki.NewClientTLSConfig(cfg.TLS.CACert, cfg.TLS.ClientCert, cfg.TLS.ClientKey)
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	// Extrai hostname para ServerName (necessário para validação TLS)
	host, _, err := net.SplitHostPort(cfg.Server.Address)
	if err != nil {
		host = cfg.Server.Address // fallback se não tiver porta
	}
	tlsCfg.ServerName = host

	// Conecta ao server e faz handshake
	conn, sessionID, compressionMode, handshakeRTT, err := initialConnect(ctx, cfg, entry, tlsCfg, logger)
	if err != nil {
		return err
	}

	logger = logger.With("session", sessionID)

	// Persiste RTT do handshake no job para stats reporter
	if job != nil {
		job.mu.Lock()
		if job.LastResult == nil {
			job.LastResult = &BackupJobResult{}
		}
		job.LastResult.HandshakeRTT = handshakeRTT
		job.mu.Unlock()
	}

	// Rota paralela: envia ParallelInit e delega para RunParallelBackup
	if entry.Parallels > 0 {
		logger.Info("handshake successful, starting parallel pipeline", "maxStreams", entry.Parallels)

		// Envia extensão ParallelInit na conexão primária
		chunkSize := uint32(cfg.Resume.ChunkSizeRaw)
		if err := protocol.WriteParallelInit(conn, uint8(entry.Parallels), chunkSize); err != nil {
			conn.Close()
			return fmt.Errorf("writing ParallelInit: %w", err)
		}

		return runParallelBackup(ctx, cfg, entry, conn, sessionID, compressionMode, tlsCfg, logger, progress, job, controlCh)
	}

	logger.Info("handshake successful, starting resumable pipeline")

	// Envia byte discriminador 0x00 para sinalizar single-stream ao server
	// (ParallelInit começa com MaxStreams >= 1, então 0x00 = single-stream)
	if _, err := conn.Write([]byte{0x00}); err != nil {
		conn.Close()
		return fmt.Errorf("writing single-stream marker: %w", err)
	}

	// Ring buffer para backpressure e resume
	rb := NewRingBuffer(cfg.Resume.BufferSizeRaw)

	// Pipeline: scanner → tar.gz → ring buffer (produtor)
	sources := make([]string, len(entry.Sources))
	for i, s := range entry.Sources {
		sources[i] = s.Path
	}
	scanner := NewScanner(sources, entry.Exclude)

	var producerResult *StreamResult
	var producerErr error
	producerDone := make(chan struct{})

	go func() {
		defer close(producerDone)
		producerResult, producerErr = Stream(ctx, scanner, rb, progress, nil, compressionMode, entry.BandwidthLimitRaw)
		rb.Close() // sinaliza EOF para o sender
	}()

	// Sender + ACK reader loop (com resume)
	sendOffset := int64(0)
	var sendMu sync.Mutex

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Backoff exponencial
			delay := resumeBackoff * time.Duration(1<<(attempt-1))
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			logger.Info("attempting resume", "attempt", attempt, "delay", delay)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}

			// Verifica se o offset ainda está no ring buffer
			sendMu.Lock()
			currentOffset := sendOffset
			sendMu.Unlock()

			if currentOffset > 0 && !rb.Contains(currentOffset) {
				return fmt.Errorf("resume failed: offset %d no longer in ring buffer (tail=%d), restart required", currentOffset, rb.Tail())
			}

			// Reconecta e resume
			var resumeErr error
			conn, currentOffset, resumeErr = resumeConnect(ctx, cfg, entry, sessionID, tlsCfg, logger)
			if resumeErr != nil {
				logger.Warn("resume connect failed", "error", resumeErr)
				if attempt >= maxResumeAttempts {
					return fmt.Errorf("max resume attempts reached: %w", resumeErr)
				}
				continue
			}

			// Avança o offset de envio para o lastOffset do server
			sendMu.Lock()
			sendOffset = currentOffset
			sendMu.Unlock()

			// Avança o tail do ring buffer
			rb.Advance(currentOffset)
			logger.Info("resume accepted", "server_offset", currentOffset)
			attempt = 0 // reset counter on successful resume
		}

		// Sender: lê do ring buffer e escreve na conn
		senderErr := make(chan error, 1)
		senderDone := make(chan struct{})

		go func() {
			defer close(senderDone)
			buf := make([]byte, 256*1024)
			for {
				sendMu.Lock()
				offset := sendOffset
				sendMu.Unlock()

				n, err := rb.ReadAt(offset, buf)
				if err != nil {
					if err == ErrBufferClosed {
						senderErr <- nil
						return
					}
					senderErr <- err
					return
				}

				if _, err := conn.Write(buf[:n]); err != nil {
					senderErr <- fmt.Errorf("writing to conn: %w", err)
					return
				}

				sendMu.Lock()
				sendOffset += int64(n)
				sendMu.Unlock()
			}
		}()

		// ACK reader: lê SACKs do server e avança o tail
		ackDone := make(chan error, 1)

		go func() {
			for {
				sack, err := protocol.ReadSACK(conn)
				if err != nil {
					ackDone <- err
					return
				}

				rb.Advance(int64(sack.Offset))
				logger.Debug("SACK received", "offset", sack.Offset)
			}
		}()

		// Espera sender terminar ou falhar
		select {
		case <-ctx.Done():
			conn.Close()
			return ctx.Err()

		case err := <-senderErr:
			conn.Close()
			if err != nil {
				logger.Warn("sender failed, will attempt resume", "error", err)
				continue // tenta resume
			}
			// Sender terminou sem erro = produtor terminou e ring buffer fechou
		}

		// Espera produtor terminar
		<-producerDone
		if producerErr != nil {
			conn.Close()
			return fmt.Errorf("pipeline error: %w", producerErr)
		}

		// Envia trailer (checksum + size)
		logger.Info("data transfer complete",
			"bytes", producerResult.Size,
			"checksum", fmt.Sprintf("%x", producerResult.Checksum),
		)

		trailerStart := time.Now()
		if err := protocol.WriteTrailer(conn, producerResult.Checksum, producerResult.Size); err != nil {
			conn.Close()
			return fmt.Errorf("writing trailer: %w", err)
		}

		// Lê Final ACK diretamente da conn (o ACK reader lerá erro e terminará)
		finalACK, err := protocol.ReadFinalACK(conn)
		finalACKRTT := time.Since(trailerStart)
		if err != nil {
			conn.Close()
			return fmt.Errorf("reading final ACK: %w", err)
		}

		logger.Info("final ACK received", "final_ack_rtt", finalACKRTT)

		conn.Close()

		switch finalACK.Status {
		case protocol.FinalStatusOK:
			logger.Info("backup completed successfully",
				"bytes", producerResult.Size,
			)
			return nil
		case protocol.FinalStatusChecksumMismatch:
			return fmt.Errorf("server reported checksum mismatch")
		case protocol.FinalStatusWriteError:
			return fmt.Errorf("server reported write error")
		default:
			return fmt.Errorf("server returned unknown status: %d", finalACK.Status)
		}
	}
}

// initialConnect realiza a conexão inicial e handshake.
// Retorna a conexão, sessionID e o RTT do handshake.
func initialConnect(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, tlsCfg *tls.Config, logger *slog.Logger) (net.Conn, string, byte, time.Duration, error) {
	conn, err := dialWithContext(ctx, cfg.Server.Address, tlsCfg)
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("connecting to server: %w", err)
	}

	logger.Info("connected to server", "address", cfg.Server.Address)

	// Handshake com medição de RTT
	handshakeStart := time.Now()
	// Envia handshake
	agentVersion := Version
	if err := protocol.WriteHandshake(conn, cfg.Agent.Name, entry.Storage, entry.Name, agentVersion); err != nil {
		conn.Close()
		return nil, "", 0, 0, fmt.Errorf("writing handshake: %w", err)
	}

	ack, err := protocol.ReadACK(conn)
	handshakeRTT := time.Since(handshakeStart)
	if err != nil {
		conn.Close()
		return nil, "", 0, 0, fmt.Errorf("reading handshake ACK: %w", err)
	}

	logger.Info("handshake ACK received", "handshake_rtt", handshakeRTT)

	if ack.Status != protocol.StatusGo {
		conn.Close()
		return nil, "", 0, 0, fmt.Errorf("server rejected backup: status=%d message=%q", ack.Status, ack.Message)
	}

	return conn, ack.SessionID, ack.CompressionMode, handshakeRTT, nil
}

// resumeConnect reconecta e envia RESUME para o server.
// Retorna a conexão, o lastOffset do server e o RTT do resume.
func resumeConnect(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, sessionID string, tlsCfg *tls.Config, logger *slog.Logger) (net.Conn, int64, error) {
	conn, err := dialWithContext(ctx, cfg.Server.Address, tlsCfg)
	if err != nil {
		return nil, 0, fmt.Errorf("reconnecting: %w", err)
	}

	// Envia RESUME com medição de RTT
	resumeStart := time.Now()
	if err := protocol.WriteResume(conn, sessionID, cfg.Agent.Name, entry.Storage); err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("writing resume: %w", err)
	}

	rACK, err := protocol.ReadResumeACK(conn)
	resumeRTT := time.Since(resumeStart)
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("reading resume ACK: %w", err)
	}

	logger.Info("resume ACK received", "resume_rtt", resumeRTT)

	if rACK.Status != protocol.ResumeStatusOK {
		conn.Close()
		return nil, 0, fmt.Errorf("server rejected resume: status=%d", rACK.Status)
	}

	return conn, int64(rACK.LastOffset), nil
}

// dialWithContext conecta via TLS respeitando o contexto para cancelamento.
func dialWithContext(ctx context.Context, address string, tlsCfg *tls.Config) (*tls.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(conn, tlsCfg)

	// Realiza o handshake TLS com deadline baseado no context
	if deadline, ok := ctx.Deadline(); ok {
		tlsConn.SetDeadline(deadline)
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	// Remove deadline após handshake (o stream não deve ter timeout fixo)
	tlsConn.SetDeadline(time.Time{})

	return tlsConn, nil
}

// runParallelBackup executa o pipeline de backup com streams paralelos.
// A conn primária é usada apenas como canal de controle (Trailer + FinalACK).
// Todas as N streams de dados conectam ao server via ParallelJoin.
func runParallelBackup(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, conn net.Conn, sessionID string, compressionMode byte, tlsCfg *tls.Config, logger *slog.Logger, progress *ProgressReporter, job *BackupJob, controlCh *ControlChannel) error {
	defer conn.Close()

	// Callback para atualizar o progress reporter e job metrics com streams ativos
	var onStreamChange func(active, max int)
	onStreamChange = func(active, max int) {
		if progress != nil {
			progress.SetStreams(active, max)
		}
		if job != nil {
			atomic.StoreInt32(&job.ActiveStreams, int32(active))
			atomic.StoreInt32(&job.MaxStreams, int32(max))
		}
	}

	// Cria dispatcher — conn primária é control-only (não usada para dados)
	dispatcher := NewDispatcher(DispatcherConfig{
		MaxStreams:     entry.Parallels,
		BufferSize:     cfg.Resume.BufferSizeRaw,
		ChunkSize:      int(cfg.Resume.ChunkSizeRaw),
		SessionID:      sessionID,
		ServerAddr:     cfg.Server.Address,
		TLSConfig:      tlsCfg,
		AgentName:      cfg.Agent.Name,
		StorageName:    entry.Storage,
		Logger:         logger,
		PrimaryConn:    conn,
		OnStreamChange: onStreamChange,
	})
	defer dispatcher.Close()

	// Ativa todas as N streams via ParallelJoin (incluindo stream 0).
	// Cada stream tem seu próprio sender com retry + ACK reader.
	// Streams que falharem no connect são logados mas não impedem o backup.
	activatedCount := 0
	for i := 0; i < entry.Parallels; i++ {
		if err := dispatcher.ActivateStream(i); err != nil {
			logger.Warn("failed to activate parallel stream, continuing with fewer streams",
				"stream", i, "error", err)
		} else {
			activatedCount++
		}
	}

	if activatedCount == 0 {
		return fmt.Errorf("no parallel streams could be activated")
	}

	// Auto-scaler mantido apenas para scale-down em caso de subutilização
	scalerCtx, scalerCancel := context.WithCancel(ctx)
	defer scalerCancel()

	scaler := NewAutoScaler(AutoScalerConfig{
		Dispatcher: dispatcher,
		Logger:     logger,
		Mode:       entry.AutoScaler,
	})
	go scaler.Run(scalerCtx)

	// Pipeline: scanner → tar.gz → dispatcher (produtor)
	sources := make([]string, len(entry.Sources))
	for i, s := range entry.Sources {
		sources[i] = s.Path
	}
	scanner := NewScanner(sources, entry.Exclude)

	var producerResult *StreamResult
	var producerErr error
	producerDone := make(chan struct{})

	// Contadores atômicos para progresso enviado ao server via ControlChannel.
	// O PreScan roda em goroutine paralela para não bloquear o início dos streams.
	var totalObj, sentObj atomic.Uint32
	var walkDone atomic.Int32

	// onObject callback: incrementa sentObj a cada objeto processado pelo Stream()
	var onObject func()

	if controlCh != nil {
		onObject = func() {
			sentObj.Add(1)
		}

		// PreScan em goroutine para calcular total de objetos sem bloquear o backup
		go func() {
			preScanSources := make([]string, len(entry.Sources))
			for i, s := range entry.Sources {
				preScanSources[i] = s.Path
			}
			preScanScanner := NewScanner(preScanSources, entry.Exclude)
			stats, err := preScanScanner.PreScan(ctx)
			if err != nil {
				logger.Warn("pre-scan for progress failed", "error", err)
				return
			}
			totalObj.Store(uint32(stats.TotalObjects))
			walkDone.Store(1)
			logger.Info("pre-scan for progress complete", "total_objects", stats.TotalObjects)
		}()

		controlCh.SetProgressProvider(func() (uint32, uint32, bool) {
			return totalObj.Load(), sentObj.Load(), walkDone.Load() != 0
		})
		defer controlCh.SetProgressProvider(nil)

		controlCh.SetAutoScaleStatsProvider(func() *protocol.ControlAutoScaleStats {
			snap := scaler.Snapshot()
			probeActive := uint8(0)
			if snap.ProbeActive {
				probeActive = 1
			}
			return &protocol.ControlAutoScaleStats{
				Efficiency:    snap.Efficiency,
				ProducerMBs:   snap.ProducerMBs,
				DrainMBs:      snap.DrainMBs,
				ActiveStreams: snap.ActiveStreams,
				MaxStreams:    snap.MaxStreams,
				State:         snap.State,
				ProbeActive:   probeActive,
			}
		})
		defer controlCh.SetAutoScaleStatsProvider(nil)
	}

	go func() {
		defer close(producerDone)
		producerResult, producerErr = Stream(ctx, scanner, dispatcher, progress, onObject, compressionMode, entry.BandwidthLimitRaw)
		dispatcher.Flush() // emite chunk parcial pendente no buffer de acumulação
		dispatcher.Close() // sinaliza EOF para todos os senders
	}()

	// WaitAllSenders e produtor rodam em paralelo.
	// Context com timeout previne deadlock eterno.
	sendersCtx, sendersCancel := context.WithTimeout(ctx, MaxBackupDuration)
	defer sendersCancel()

	sendersDone := make(chan error, 1)
	go func() {
		sendersDone <- dispatcher.WaitAllSenders(sendersCtx)
	}()

	// Espera ambos: senders E produtor
	var sendersErr error
	select {
	case sendersErr = <-sendersDone:
		// Senders terminaram — esperamos o produtor
		<-producerDone
	case <-producerDone:
		// Produtor terminou primeiro — espera senders (já deve estar perto de terminar)
		sendersErr = <-sendersDone
	}

	scalerCancel()

	if sendersErr != nil {
		return fmt.Errorf("parallel sender error: %w", sendersErr)
	}
	if producerErr != nil {
		return fmt.Errorf("parallel pipeline error: %w", producerErr)
	}

	// Sinaliza ao server que toda a ingestão foi completada com sucesso.
	// O server espera este frame antes de prosseguir com Finalize().
	if controlCh != nil {
		if err := controlCh.SendIngestionDone(sessionID); err != nil {
			logger.Error("failed to send ControlIngestionDone — server may timeout waiting", "error", err)
			return fmt.Errorf("signaling ingestion done: %w", err)
		}
		logger.Info("sent ControlIngestionDone to server", "session", sessionID)
	}

	// Envia Trailer direto pela conn primária (sem ChunkHeader framing).
	// A conn primária nunca enviou dados, então não há conflito de framing.
	trailerStart := time.Now()
	if err := protocol.WriteTrailer(conn, producerResult.Checksum, producerResult.Size); err != nil {
		return fmt.Errorf("writing trailer: %w", err)
	}

	// Lê Final ACK
	finalACK, err := protocol.ReadFinalACK(conn)
	finalACKRTT := time.Since(trailerStart)
	if err != nil {
		return fmt.Errorf("reading final ACK: %w", err)
	}

	logger.Info("final ACK received", "final_ack_rtt", finalACKRTT)

	switch finalACK.Status {
	case protocol.FinalStatusOK:
		logger.Info("parallel backup completed successfully",
			"bytes", producerResult.Size,
			"streams", entry.Parallels,
		)
		return nil
	case protocol.FinalStatusChecksumMismatch:
		return fmt.Errorf("server reported checksum mismatch")
	case protocol.FinalStatusWriteError:
		return fmt.Errorf("server reported write error")
	default:
		return fmt.Errorf("server returned unknown status: %d", finalACK.Status)
	}
}

// teeWriter é um io.Writer que escreve em ambos os destinos.
// Usado para escrever no ring buffer E no hash ao mesmo tempo.
type teeWriter struct {
	a, b io.Writer
}

func (tw *teeWriter) Write(p []byte) (int, error) {
	n, err := tw.a.Write(p)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return tw.b.Write(p)
}
