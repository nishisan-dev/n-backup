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
	globalSeq   uint32 // sequência global de chunks para reconstrução no server
	sessionID   string
	serverAddr  string
	tlsCfg      *tls.Config
	agentName   string
	storageName string
	logger      *slog.Logger

	// Callback invocado quando streams mudam (ativação/desativação)
	onStreamChange func(active, max int)

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
	MaxStreams     int
	BufferSize     int64
	ChunkSize      int
	SessionID      string
	ServerAddr     string
	TLSConfig      *tls.Config
	AgentName      string
	StorageName    string
	Logger         *slog.Logger
	PrimaryConn    net.Conn              // conexão primária (stream 0, já autenticada)
	OnStreamChange func(active, max int) // callback para notificar mudanças de streams
}

// NewDispatcher cria um novo Dispatcher com o stream primário ativo.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	d := &Dispatcher{
		streams:        make([]*ParallelStream, cfg.MaxStreams),
		maxStreams:     cfg.MaxStreams,
		chunkSize:      cfg.ChunkSize,
		sessionID:      cfg.SessionID,
		serverAddr:     cfg.ServerAddr,
		tlsCfg:         cfg.TLSConfig,
		agentName:      cfg.AgentName,
		storageName:    cfg.StorageName,
		logger:         cfg.Logger,
		onStreamChange: cfg.OnStreamChange,
		lastSampleAt:   time.Now(),
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

	// Notifica callback inicial (1 stream ativo)
	d.notifyStreamChange()

	return d
}

// Write implementa io.Writer. Distribui chunks em round-robin pelos streams ativos.
// Cada chunk é precedido por um ChunkHeader (GlobalSeq + Length) no ring buffer,
// permitindo ao server reconstruir a ordem global dos chunks.
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
		seq := d.globalSeq
		d.globalSeq++
		d.nextStream++
		d.mu.Unlock()

		stream := d.streams[streamIdx]

		// Escreve ChunkHeader (8 bytes) no ring buffer antes dos dados
		hdr := make([]byte, 8)
		hdr[0] = byte(seq >> 24)
		hdr[1] = byte(seq >> 16)
		hdr[2] = byte(seq >> 8)
		hdr[3] = byte(seq)
		l := uint32(chunk)
		hdr[4] = byte(l >> 24)
		hdr[5] = byte(l >> 16)
		hdr[6] = byte(l >> 8)
		hdr[7] = byte(l)

		if _, err := stream.rb.Write(hdr); err != nil {
			return written, fmt.Errorf("writing chunk header to stream %d ring buffer: %w", streamIdx, err)
		}

		// Escreve os dados do chunk no ring buffer
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

// StartSelfDrainingSender inicia um sender que auto-avança o ring buffer após cada write.
// Usado para stream 0 onde não há ACK reader (a mesma conn é usada para FinalACK).
// Sem Advance(), o ring buffer enche e o produtor bloqueia para sempre.
func (d *Dispatcher) StartSelfDrainingSender(streamIdx int) {
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
			newOffset := stream.sendOffset
			stream.sendMu.Unlock()

			// Auto-advance: libera espaço no ring buffer imediatamente.
			// Seguro para stream 0 pois resume não é suportado em modo paralelo.
			stream.rb.Advance(newOffset)
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
	d.notifyStreamChange()
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
	d.notifyStreamChange()
}

// ActiveStreams retorna o número de streams ativos.
func (d *Dispatcher) ActiveStreams() int {
	return int(atomic.LoadInt32(&d.activeCount))
}

// notifyStreamChange invoca o callback de mudança de streams, se configurado.
func (d *Dispatcher) notifyStreamChange() {
	if d.onStreamChange != nil {
		d.onStreamChange(d.ActiveStreams(), d.maxStreams)
	}
}

// RateSample contém as taxas calculadas em um único ponto no tempo.
// Elimina a race condition de calcular elapsed separadamente para cada métrica.
type RateSample struct {
	ProducerBps float64 // bytes/s do produtor
	DrainBps    float64 // bytes/s drenados (soma de todos os streams)
}

// SampleRates captura as taxas do produtor e drain em um único instante,
// usando o mesmo elapsed para ambas. Reseta todos os contadores atomicamente.
func (d *Dispatcher) SampleRates() RateSample {
	d.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(d.lastSampleAt).Seconds()
	d.lastSampleAt = now
	d.mu.Unlock()

	if elapsed <= 0 {
		return RateSample{}
	}

	// Swap-and-reset dos contadores atômicos
	producerBytes := atomic.SwapInt64(&d.producerBytes, 0)

	var totalDrain int64
	for i := 0; i < d.maxStreams; i++ {
		if d.streams[i].active {
			totalDrain += atomic.SwapInt64(&d.streams[i].drainBytes, 0)
		}
	}

	return RateSample{
		ProducerBps: float64(producerBytes) / elapsed,
		DrainBps:    float64(totalDrain) / elapsed,
	}
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
