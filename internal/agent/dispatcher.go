// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// Dispatcher distribui chunks de dados em round-robin por N streams paralelos.
// Implementa io.Writer para ser usado como destino do pipeline tar.gz.
// Cada stream tem seu próprio RingBuffer, sender goroutine e ACK reader.
type Dispatcher struct {
	streams     []*ParallelStream
	maxStreams  int
	activeCount int32 // atomic
	chunkSize   int
	nextStream  int
	sessionID   string
	serverAddr  string
	tlsCfg      *tls.Config
	agentName   string
	storageName string
	logger      *slog.Logger

	// Métricas para o auto-scaler
	producerBytes int64 // atomic — total de bytes recebidos pelo Write
	lastSampleAt  time.Time
	mu            sync.Mutex
}

// ParallelStream representa um stream individual com seu ring buffer e conexão.
type ParallelStream struct {
	index      uint8
	rb         *RingBuffer
	conn       net.Conn
	sendOffset int64
	sendMu     sync.Mutex
	drainBytes int64 // atomic — bytes drenados (ACK'd) por este stream
	active     bool
	senderDone chan struct{}
	senderErr  chan error
}

// DispatcherConfig contém os parâmetros para criar um Dispatcher.
type DispatcherConfig struct {
	MaxStreams  int
	BufferSize  int64
	ChunkSize   int
	SessionID   string
	ServerAddr  string
	TLSConfig   *tls.Config
	AgentName   string
	StorageName string
	Logger      *slog.Logger
	PrimaryConn net.Conn // conexão primária (stream 0, já autenticada)
}

// NewDispatcher cria um novo Dispatcher com o stream primário ativo.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	d := &Dispatcher{
		streams:      make([]*ParallelStream, cfg.MaxStreams),
		maxStreams:   cfg.MaxStreams,
		chunkSize:    cfg.ChunkSize,
		sessionID:    cfg.SessionID,
		serverAddr:   cfg.ServerAddr,
		tlsCfg:       cfg.TLSConfig,
		agentName:    cfg.AgentName,
		storageName:  cfg.StorageName,
		logger:       cfg.Logger,
		lastSampleAt: time.Now(),
	}

	// Inicializa todos os streams com ring buffers
	for i := 0; i < cfg.MaxStreams; i++ {
		d.streams[i] = &ParallelStream{
			index:      uint8(i),
			rb:         NewRingBuffer(cfg.BufferSize),
			active:     false,
			senderDone: make(chan struct{}),
			senderErr:  make(chan error, 1),
		}
	}

	// Stream 0 usa a conexão primária (já autenticada via handshake)
	d.streams[0].conn = cfg.PrimaryConn
	d.streams[0].active = true
	atomic.StoreInt32(&d.activeCount, 1)

	return d
}

// Write implementa io.Writer. Distribui chunks em round-robin pelos streams ativos.
// Se um chunk é menor que chunkSize, ainda é enviado (último chunk do stream).
func (d *Dispatcher) Write(p []byte) (int, error) {
	written := 0
	active := int(atomic.LoadInt32(&d.activeCount))

	for written < len(p) {
		// Calcula tamanho do chunk
		remaining := len(p) - written
		chunk := d.chunkSize
		if remaining < chunk {
			chunk = remaining
		}

		// Seleciona stream em round-robin (apenas ativos)
		d.mu.Lock()
		streamIdx := d.nextStream % active
		d.nextStream++
		d.mu.Unlock()

		stream := d.streams[streamIdx]

		// Escreve no ring buffer do stream selecionado
		n, err := stream.rb.Write(p[written : written+chunk])
		if err != nil {
			return written, fmt.Errorf("writing to stream %d ring buffer: %w", streamIdx, err)
		}
		written += n
	}

	atomic.AddInt64(&d.producerBytes, int64(written))
	return written, nil
}

// StartSender inicia a goroutine sender para um stream específico.
// Lê do ring buffer e escreve na conexão TCP.
func (d *Dispatcher) StartSender(streamIdx int) {
	stream := d.streams[streamIdx]

	go func() {
		defer close(stream.senderDone)
		buf := make([]byte, 256*1024)
		for {
			stream.sendMu.Lock()
			offset := stream.sendOffset
			stream.sendMu.Unlock()

			n, err := stream.rb.ReadAt(offset, buf)
			if err != nil {
				if err == ErrBufferClosed {
					stream.senderErr <- nil
					return
				}
				stream.senderErr <- err
				return
			}

			if _, err := stream.conn.Write(buf[:n]); err != nil {
				stream.senderErr <- fmt.Errorf("writing to stream %d conn: %w", streamIdx, err)
				return
			}

			stream.sendMu.Lock()
			stream.sendOffset += int64(n)
			stream.sendMu.Unlock()
		}
	}()
}

// StartACKReader inicia a goroutine que lê ChunkSACKs do server para um stream.
func (d *Dispatcher) StartACKReader(streamIdx int) {
	stream := d.streams[streamIdx]

	go func() {
		for {
			csack, err := protocol.ReadChunkSACK(stream.conn)
			if err != nil {
				return // conn fechada ou erro
			}

			stream.rb.Advance(int64(csack.Offset))
			atomic.AddInt64(&stream.drainBytes, int64(csack.Offset))
			d.logger.Debug("ChunkSACK received",
				"stream", streamIdx,
				"chunkSeq", csack.ChunkSeq,
				"offset", csack.Offset,
			)
		}
	}()
}

// ActivateStream ativa um stream adicional conectando ao server via ParallelJoin.
func (d *Dispatcher) ActivateStream(streamIdx int) error {
	if streamIdx <= 0 || streamIdx >= d.maxStreams {
		return fmt.Errorf("invalid stream index %d", streamIdx)
	}

	stream := d.streams[streamIdx]
	if stream.active {
		return nil // já ativo
	}

	// Conecta ao server
	dialer := &net.Dialer{}
	rawConn, err := dialer.Dial("tcp", d.serverAddr)
	if err != nil {
		return fmt.Errorf("connecting stream %d: %w", streamIdx, err)
	}

	tlsConn := tls.Client(rawConn, d.tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return fmt.Errorf("TLS handshake stream %d: %w", streamIdx, err)
	}
	tlsConn.SetDeadline(time.Time{}) // sem timeout no stream

	// Envia ParallelJoin
	if err := protocol.WriteParallelJoin(tlsConn, d.sessionID, uint8(streamIdx)); err != nil {
		tlsConn.Close()
		return fmt.Errorf("writing ParallelJoin stream %d: %w", streamIdx, err)
	}

	// Lê ACK
	status, err := protocol.ReadParallelACK(tlsConn)
	if err != nil {
		tlsConn.Close()
		return fmt.Errorf("reading ParallelACK stream %d: %w", streamIdx, err)
	}
	if status != protocol.ParallelStatusOK {
		tlsConn.Close()
		return fmt.Errorf("server rejected ParallelJoin stream %d: status=%d", streamIdx, status)
	}

	stream.conn = tlsConn
	stream.active = true
	atomic.AddInt32(&d.activeCount, 1)

	// Inicia sender e ACK reader
	d.StartSender(streamIdx)
	d.StartACKReader(streamIdx)

	d.logger.Info("parallel stream activated", "stream", streamIdx)
	return nil
}

// DeactivateStream desativa um stream (para de receber chunks novos).
func (d *Dispatcher) DeactivateStream(streamIdx int) {
	if streamIdx <= 0 || streamIdx >= d.maxStreams {
		return
	}

	stream := d.streams[streamIdx]
	if !stream.active {
		return
	}

	stream.active = false
	atomic.AddInt32(&d.activeCount, -1)
	d.logger.Info("parallel stream deactivated", "stream", streamIdx)
}

// ActiveStreams retorna o número de streams ativos.
func (d *Dispatcher) ActiveStreams() int {
	return int(atomic.LoadInt32(&d.activeCount))
}

// ProducerRate retorna bytes/s do produtor desde a última amostra.
func (d *Dispatcher) ProducerRate() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(d.lastSampleAt).Seconds()
	if elapsed <= 0 {
		return 0
	}

	bytes := atomic.LoadInt64(&d.producerBytes)
	rate := float64(bytes) / elapsed

	// Reset para próxima amostra
	atomic.StoreInt64(&d.producerBytes, 0)
	d.lastSampleAt = now

	return rate
}

// DrainRate retorna a soma de bytes/s drenados por todos os streams ativos.
func (d *Dispatcher) DrainRate() float64 {
	var totalDrain int64
	for i := 0; i < d.maxStreams; i++ {
		if d.streams[i].active {
			totalDrain += atomic.LoadInt64(&d.streams[i].drainBytes)
			atomic.StoreInt64(&d.streams[i].drainBytes, 0)
		}
	}

	d.mu.Lock()
	elapsed := time.Since(d.lastSampleAt).Seconds()
	d.mu.Unlock()

	if elapsed <= 0 {
		return 0
	}
	return float64(totalDrain) / elapsed
}

// Close fecha todos os ring buffers e conexões.
func (d *Dispatcher) Close() {
	for i := 0; i < d.maxStreams; i++ {
		d.streams[i].rb.Close()
		if d.streams[i].conn != nil && i > 0 {
			// Stream 0 é gerenciado pelo caller (backup.go)
			d.streams[i].conn.Close()
		}
	}
}

// WaitSender espera que o sender do stream especificado termine.
// Retorna o erro do sender, ou nil se terminou normalmente (EOF).
func (d *Dispatcher) WaitSender(streamIdx int) error {
	stream := d.streams[streamIdx]
	<-stream.senderDone
	select {
	case err := <-stream.senderErr:
		return err
	default:
		return nil
	}
}

// WaitAllSenders espera que todos os senders ativos terminem.
func (d *Dispatcher) WaitAllSenders() error {
	for i := 0; i < d.maxStreams; i++ {
		if d.streams[i].active {
			if err := d.WaitSender(i); err != nil {
				return fmt.Errorf("stream %d sender error: %w", i, err)
			}
		}
	}
	return nil
}
