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
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// maxResumeAttempts é o número máximo de tentativas de resume antes de reiniciar.
const maxResumeAttempts = 5

// resumeBackoff é o tempo inicial entre tentativas de resume.
const resumeBackoff = 2 * time.Second

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
func RunBackup(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger, progress *ProgressReporter) error {
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
	conn, sessionID, err := initialConnect(ctx, cfg, entry, tlsCfg, logger)
	if err != nil {
		return err
	}

	logger = logger.With("session", sessionID)

	// Rota paralela: envia ParallelInit e delega para RunParallelBackup
	if entry.Parallels > 0 {
		logger.Info("handshake successful, starting parallel pipeline", "maxStreams", entry.Parallels)

		// Envia extensão ParallelInit na conexão primária
		chunkSize := uint32(cfg.Resume.BufferSizeRaw)
		if err := protocol.WriteParallelInit(conn, uint8(entry.Parallels), chunkSize); err != nil {
			conn.Close()
			return fmt.Errorf("writing ParallelInit: %w", err)
		}

		return runParallelBackup(ctx, cfg, entry, conn, sessionID, tlsCfg, logger, progress)
	}

	logger.Info("handshake successful, starting resumable pipeline")

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
		producerResult, producerErr = Stream(ctx, scanner, rb, progress)
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

		if err := protocol.WriteTrailer(conn, producerResult.Checksum, producerResult.Size); err != nil {
			conn.Close()
			return fmt.Errorf("writing trailer: %w", err)
		}

		// Lê Final ACK diretamente da conn (o ACK reader lerá erro e terminará)
		finalACK, err := protocol.ReadFinalACK(conn)
		if err != nil {
			conn.Close()
			return fmt.Errorf("reading final ACK: %w", err)
		}

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
func initialConnect(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, tlsCfg *tls.Config, logger *slog.Logger) (net.Conn, string, error) {
	conn, err := dialWithContext(ctx, cfg.Server.Address, tlsCfg)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to server: %w", err)
	}

	logger.Info("connected to server", "address", cfg.Server.Address)

	// Handshake
	if err := protocol.WriteHandshake(conn, cfg.Agent.Name, entry.Storage); err != nil {
		conn.Close()
		return nil, "", fmt.Errorf("writing handshake: %w", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		conn.Close()
		return nil, "", fmt.Errorf("reading handshake ACK: %w", err)
	}

	if ack.Status != protocol.StatusGo {
		conn.Close()
		return nil, "", fmt.Errorf("server rejected backup: status=%d message=%q", ack.Status, ack.Message)
	}

	return conn, ack.SessionID, nil
}

// resumeConnect reconecta e envia RESUME para o server.
// Retorna a conexão e o lastOffset do server.
func resumeConnect(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, sessionID string, tlsCfg *tls.Config, logger *slog.Logger) (net.Conn, int64, error) {
	conn, err := dialWithContext(ctx, cfg.Server.Address, tlsCfg)
	if err != nil {
		return nil, 0, fmt.Errorf("reconnecting: %w", err)
	}

	// Envia RESUME
	if err := protocol.WriteResume(conn, sessionID, cfg.Agent.Name, entry.Storage); err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("writing resume: %w", err)
	}

	rACK, err := protocol.ReadResumeACK(conn)
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("reading resume ACK: %w", err)
	}

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
// O stream 0 já está conectado (conn). Streams adicionais são abertos pelo auto-scaler.
func runParallelBackup(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, conn net.Conn, sessionID string, tlsCfg *tls.Config, logger *slog.Logger, progress *ProgressReporter) error {
	defer conn.Close()

	// Cria dispatcher com stream primário
	dispatcher := NewDispatcher(DispatcherConfig{
		MaxStreams:  entry.Parallels,
		BufferSize:  cfg.Resume.BufferSizeRaw,
		ChunkSize:   int(cfg.Resume.BufferSizeRaw), // chunk = buffer_size
		SessionID:   sessionID,
		ServerAddr:  cfg.Server.Address,
		TLSConfig:   tlsCfg,
		AgentName:   cfg.Agent.Name,
		StorageName: entry.Storage,
		Logger:      logger,
		PrimaryConn: conn,
	})
	defer dispatcher.Close()

	// Inicia sender e ACK reader para stream 0
	dispatcher.StartSender(0)
	// Nao iniciar ACK reader no stream 0: este mesmo conn e usado para FinalACK.
	// Um reader concorrente pode consumir o byte do FinalACK e causar EOF espurio.

	// Inicia auto-scaler
	scalerCtx, scalerCancel := context.WithCancel(ctx)
	defer scalerCancel()

	scaler := NewAutoScaler(AutoScalerConfig{
		Dispatcher: dispatcher,
		Logger:     logger,
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

	go func() {
		defer close(producerDone)
		producerResult, producerErr = Stream(ctx, scanner, dispatcher, progress)
		dispatcher.Close() // sinaliza EOF para todos os senders
	}()

	// Espera todos os senders terminarem
	if err := dispatcher.WaitAllSenders(); err != nil {
		scalerCancel()
		<-producerDone
		return fmt.Errorf("parallel sender error: %w", err)
	}

	// Espera produtor terminar
	<-producerDone
	scalerCancel()

	if producerErr != nil {
		return fmt.Errorf("parallel pipeline error: %w", producerErr)
	}

	// Envia trailer pela conexão primária (stream 0)
	logger.Info("parallel data transfer complete",
		"bytes", producerResult.Size,
		"checksum", fmt.Sprintf("%x", producerResult.Checksum),
		"streams", dispatcher.ActiveStreams(),
	)

	if err := protocol.WriteTrailer(conn, producerResult.Checksum, producerResult.Size); err != nil {
		return fmt.Errorf("writing trailer: %w", err)
	}

	// Sinaliza EOF para o server (TLS close_notify) — server lê até EOF,
	// extrai trailer dos últimos 44 bytes. Sem isso, server bloqueia em Read().
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.CloseWrite(); err != nil {
			return fmt.Errorf("closing write side: %w", err)
		}
	}

	// Lê Final ACK
	finalACK, err := protocol.ReadFinalACK(conn)
	if err != nil {
		return fmt.Errorf("reading final ACK: %w", err)
	}

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
