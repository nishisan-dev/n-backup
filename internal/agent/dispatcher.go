// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

const (
	// writeDeadline é o timeout aplicado a cada conn.Write para detectar conexões half-open.
	// Deve ser compatível com o streamReadDeadline do server (30s) para que falhas
	// sejam detectadas rapidamente em ambos os lados.
	writeDeadline = 30 * time.Second

	// maxRetriesPerStream é o número máximo de tentativas de reconexão por stream.
	maxRetriesPerStream = 5

	// baseBackoff é o intervalo base para backoff exponencial em reconexões.
	baseBackoff = 1 * time.Second

	// maxBackoff é o teto do backoff exponencial.
	maxBackoff = 30 * time.Second
)

// ErrAllStreamsDead indica que todos os streams paralelos morreram permanentemente.
var ErrAllStreamsDead = errors.New("all parallel streams are permanently dead")

// Dispatcher distribui chunks de dados em round-robin por N streams paralelos.
// Implementa io.Writer para ser usado como destino do pipeline tar.gz.
// Cada stream tem seu próprio RingBuffer, sender goroutine com retry e ACK reader.
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

	// Buffer de acumulação: dados são coletados aqui até completar chunkSize,
	// momento em que um chunk completo é emitido para o ring buffer do stream.
	pending    []byte
	pendingLen int

	// Callback invocado quando streams mudam (ativação/desativação)
	onStreamChange func(active, max int)

	// DSCP code point para marcar packets (0=desabilitado)
	dscpValue int

	// Métricas para o auto-scaler
	producerBytes int64 // atomic — total de bytes recebidos pelo Write
	lastSampleAt  time.Time
	mu            sync.Mutex

	// Diagnóstico producer/consumer: identifica gargalo
	producerBlockedNs int64 // atomic — ns que o producer ficou bloqueado em rb.Write (buffer cheio = rede lenta)
	senderIdleNs      int64 // atomic — ns que os senders ficaram bloqueados em rb.ReadAt (buffer vazio = producer lento)
}

// ParallelStream representa um stream individual com seu ring buffer e conexão.
type ParallelStream struct {
	index      uint8
	rb         *RingBuffer
	conn       net.Conn
	connMu     sync.Mutex // protege conn durante reconnect
	sendOffset int64
	sendMu     sync.Mutex
	drainBytes int64 // atomic — bytes drenados (ACK'd) por este stream
	active     bool
	dead       bool // permanentemente morto (esgotou retries)
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
	PrimaryConn    net.Conn              // conexão primária (control-only, usada apenas para Trailer+FinalACK)
	OnStreamChange func(active, max int) // callback para notificar mudanças de streams
	DSCPValue      int                   // DSCP code point (0=desabilitado)
}

// NewDispatcher cria um novo Dispatcher.
// A conn primária não é usada para dados — todas as N streams conectam via ParallelJoin.
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
		dscpValue:      cfg.DSCPValue,
		lastSampleAt:   time.Now(),
		pending:        make([]byte, cfg.ChunkSize),
		pendingLen:     0,
	}

	// Inicializa todos os streams com ring buffers (inativos)
	for i := 0; i < cfg.MaxStreams; i++ {
		d.streams[i] = &ParallelStream{
			index:      uint8(i),
			rb:         NewRingBuffer(cfg.BufferSize),
			active:     false,
			dead:       false,
			senderDone: make(chan struct{}),
			senderErr:  make(chan error, 1),
		}
	}

	return d
}

// Write implementa io.Writer. Acumula dados no buffer interno (pending) e emite
// chunks completos de chunkSize bytes para os ring buffers dos streams em round-robin.
// Cada chunk é precedido por um ChunkHeader (GlobalSeq + Length) no ring buffer,
// permitindo ao server reconstruir a ordem global dos chunks.
func (d *Dispatcher) Write(p []byte) (int, error) {
	totalWritten := 0

	for totalWritten < len(p) {
		// Quanto espaço resta no buffer pending?
		space := d.chunkSize - d.pendingLen
		toCopy := len(p) - totalWritten
		if toCopy > space {
			toCopy = space
		}

		// Copia dados para o buffer pending
		copy(d.pending[d.pendingLen:], p[totalWritten:totalWritten+toCopy])
		d.pendingLen += toCopy
		totalWritten += toCopy

		// Se o buffer está cheio, emite um chunk completo
		if d.pendingLen == d.chunkSize {
			if err := d.emitChunk(d.pending[:d.pendingLen]); err != nil {
				return totalWritten, err
			}
			d.pendingLen = 0
		}
	}

	atomic.AddInt64(&d.producerBytes, int64(totalWritten))
	return totalWritten, nil
}

// Flush emite o chunk parcial pendente (se houver).
// Deve ser chamado antes de Close para garantir que todos os dados foram enviados.
func (d *Dispatcher) Flush() error {
	if d.pendingLen > 0 {
		if err := d.emitChunk(d.pending[:d.pendingLen]); err != nil {
			return err
		}
		d.pendingLen = 0
	}
	return nil
}

// emitChunk envia um chunk completo para o ring buffer do próximo stream ativo em round-robin.
// Skippa streams mortos ou inativos. Retorna ErrAllStreamsDead se nenhum stream está disponível.
func (d *Dispatcher) emitChunk(data []byte) error {
	d.mu.Lock()
	seq := d.globalSeq
	d.globalSeq++

	// Procura um stream ativo (round-robin com skip de inativos/mortos)
	var stream *ParallelStream
	for attempts := 0; attempts < d.maxStreams; attempts++ {
		idx := d.nextStream % d.maxStreams
		d.nextStream++
		s := d.streams[idx]
		if s.active && !s.dead {
			stream = s
			break
		}
	}
	d.mu.Unlock()

	if stream == nil {
		return ErrAllStreamsDead
	}

	// Escreve ChunkHeader (8 bytes) no ring buffer antes dos dados
	hdr := make([]byte, 8)
	hdr[0] = byte(seq >> 24)
	hdr[1] = byte(seq >> 16)
	hdr[2] = byte(seq >> 8)
	hdr[3] = byte(seq)
	l := uint32(len(data))
	hdr[4] = byte(l >> 24)
	hdr[5] = byte(l >> 16)
	hdr[6] = byte(l >> 8)
	hdr[7] = byte(l)

	// Diagnóstico: mede tempo bloqueado em rb.Write (indica buffer cheio = rede lenta)
	writeStart := time.Now()
	if _, err := stream.rb.Write(hdr); err != nil {
		return fmt.Errorf("writing chunk header to stream %d ring buffer: %w", stream.index, err)
	}

	// Escreve os dados do chunk no ring buffer
	if _, err := stream.rb.Write(data); err != nil {
		return fmt.Errorf("writing to stream %d ring buffer: %w", stream.index, err)
	}
	if elapsed := time.Since(writeStart); elapsed > time.Millisecond {
		atomic.AddInt64(&d.producerBlockedNs, elapsed.Nanoseconds())
	}

	return nil
}

// startSenderWithRetry inicia a goroutine sender para um stream com retry automático.
// Na falha de conn.Write, tenta reconectar via ParallelJoin com backoff exponencial.
// O server retorna lastOffset no ParallelACK, permitindo retomar da posição correta.
func (d *Dispatcher) startSenderWithRetry(streamIdx int) {
	stream := d.streams[streamIdx]

	go func() {
		defer close(stream.senderDone)
		buf := make([]byte, 256*1024)
		retries := 0

		for {
			stream.sendMu.Lock()
			offset := stream.sendOffset
			stream.sendMu.Unlock()

			// Diagnóstico: mede tempo bloqueado em rb.ReadAt (indica buffer vazio = producer lento)
			readStart := time.Now()
			n, err := stream.rb.ReadAt(offset, buf)
			if elapsed := time.Since(readStart); elapsed > time.Millisecond {
				atomic.AddInt64(&d.senderIdleNs, elapsed.Nanoseconds())
			}
			if err != nil {
				if err == ErrBufferClosed {
					stream.senderErr <- nil
					return
				}
				stream.senderErr <- err
				return
			}

			// Write com deadline para detectar conexões half-open
			stream.connMu.Lock()
			conn := stream.conn
			stream.connMu.Unlock()

			conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			_, writeErr := conn.Write(buf[:n])

			if writeErr != nil {
				d.logger.Warn("stream write failed, attempting reconnect",
					"stream", streamIdx, "error", writeErr, "retry", retries+1)

				// Tenta reconectar com backoff
				if retries >= maxRetriesPerStream {
					d.logger.Error("stream permanently dead, max retries exceeded",
						"stream", streamIdx, "retries", retries)
					stream.dead = true
					d.DeactivateStream(streamIdx)
					stream.senderErr <- fmt.Errorf("stream %d: max retries (%d) exceeded: %w",
						streamIdx, maxRetriesPerStream, writeErr)
					return
				}

				retries++
				backoff := time.Duration(math.Min(
					float64(baseBackoff)*math.Pow(2, float64(retries-1)),
					float64(maxBackoff),
				))
				d.logger.Info("backing off before reconnect",
					"stream", streamIdx, "backoff", backoff, "retry", retries)
				time.Sleep(backoff)

				// Reconnect via ParallelJoin
				resumeOffset, reconnErr := d.reconnectStream(streamIdx)
				if reconnErr != nil {
					d.logger.Error("reconnect failed",
						"stream", streamIdx, "error", reconnErr)
					// Não marca como morto ainda — continua o loop de retry
					continue
				}

				// Resume: ajusta sendOffset para o lastOffset do server
				stream.sendMu.Lock()
				stream.sendOffset = resumeOffset
				stream.sendMu.Unlock()

				d.logger.Info("stream reconnected, resuming from offset",
					"stream", streamIdx, "offset", resumeOffset)
				continue
			}

			// Write bem-sucedido — reset retries e avança offset
			retries = 0
			stream.sendMu.Lock()
			stream.sendOffset += int64(n)
			stream.sendMu.Unlock()
		}
	}()
}

// reconnectStream reconecta um stream ao server via ParallelJoin.
// Retorna o lastOffset reportado pelo server (para resume).
func (d *Dispatcher) reconnectStream(streamIdx int) (int64, error) {
	stream := d.streams[streamIdx]

	// Fecha a conexão anterior
	stream.connMu.Lock()
	if stream.conn != nil {
		stream.conn.Close()
	}
	stream.connMu.Unlock()

	// Nova conexão TLS
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	rawConn, err := dialer.Dial("tcp", d.serverAddr)
	if err != nil {
		return 0, fmt.Errorf("connecting stream %d: %w", streamIdx, err)
	}

	// Aplica DSCP marking no socket TCP (pré-TLS)
	if d.dscpValue > 0 {
		if err := ApplyDSCP(rawConn, d.dscpValue); err != nil {
			d.logger.Warn("failed to set DSCP on reconnect", "stream", streamIdx, "error", err)
		}
	}

	tlsConn := tls.Client(rawConn, d.tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return 0, fmt.Errorf("TLS handshake stream %d: %w", streamIdx, err)
	}

	// Envia ParallelJoin
	if err := protocol.WriteParallelJoin(tlsConn, d.sessionID, uint8(streamIdx)); err != nil {
		tlsConn.Close()
		return 0, fmt.Errorf("writing ParallelJoin stream %d: %w", streamIdx, err)
	}

	// Lê ParallelACK com lastOffset
	ack, err := protocol.ReadParallelACK(tlsConn)
	if err != nil {
		tlsConn.Close()
		return 0, fmt.Errorf("reading ParallelACK stream %d: %w", streamIdx, err)
	}
	if ack.Status != protocol.ParallelStatusOK {
		tlsConn.Close()
		return 0, fmt.Errorf("server rejected ParallelJoin stream %d: status=%d", streamIdx, ack.Status)
	}

	// Atualiza a conexão do stream
	stream.connMu.Lock()
	stream.conn = tlsConn
	stream.connMu.Unlock()

	// Reinicia o ACK reader para a nova conexão
	d.startACKReader(streamIdx)

	return int64(ack.LastOffset), nil
}

// startACKReader inicia a goroutine que lê ChunkSACKs do server para um stream.
func (d *Dispatcher) startACKReader(streamIdx int) {
	stream := d.streams[streamIdx]

	go func() {
		var lastOffset int64
		for {
			stream.connMu.Lock()
			conn := stream.conn
			stream.connMu.Unlock()

			csack, err := protocol.ReadChunkSACK(conn)
			if err != nil {
				return // conn fechada ou erro — sender tratará a reconexão
			}

			newOffset := int64(csack.Offset)
			stream.rb.Advance(newOffset)

			// Acumula apenas o delta (bytes novos drenados desde o último SACK)
			delta := newOffset - lastOffset
			if delta > 0 {
				atomic.AddInt64(&stream.drainBytes, delta)
			}
			lastOffset = newOffset

			d.logger.Debug("ChunkSACK received",
				"stream", streamIdx,
				"chunkSeq", csack.ChunkSeq,
				"offset", csack.Offset,
			)
		}
	}()
}

// ActivateStream ativa um stream conectando ao server via ParallelJoin.
// Suporta qualquer stream index (incluindo stream 0).
func (d *Dispatcher) ActivateStream(streamIdx int) error {
	if streamIdx < 0 || streamIdx >= d.maxStreams {
		return fmt.Errorf("invalid stream index %d", streamIdx)
	}

	stream := d.streams[streamIdx]
	if stream.active {
		return nil // já ativo
	}

	// Conecta ao server
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	rawConn, err := dialer.Dial("tcp", d.serverAddr)
	if err != nil {
		return fmt.Errorf("connecting stream %d: %w", streamIdx, err)
	}

	// Aplica DSCP marking no socket TCP (pré-TLS)
	if d.dscpValue > 0 {
		if err := ApplyDSCP(rawConn, d.dscpValue); err != nil {
			d.logger.Warn("failed to set DSCP", "stream", streamIdx, "error", err)
		}
	}

	tlsConn := tls.Client(rawConn, d.tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return fmt.Errorf("TLS handshake stream %d: %w", streamIdx, err)
	}

	// Envia ParallelJoin
	if err := protocol.WriteParallelJoin(tlsConn, d.sessionID, uint8(streamIdx)); err != nil {
		tlsConn.Close()
		return fmt.Errorf("writing ParallelJoin stream %d: %w", streamIdx, err)
	}

	// Lê ParallelACK
	ack, err := protocol.ReadParallelACK(tlsConn)
	if err != nil {
		tlsConn.Close()
		return fmt.Errorf("reading ParallelACK stream %d: %w", streamIdx, err)
	}
	if ack.Status != protocol.ParallelStatusOK {
		tlsConn.Close()
		return fmt.Errorf("server rejected ParallelJoin stream %d: status=%d", streamIdx, ack.Status)
	}

	stream.connMu.Lock()
	stream.conn = tlsConn
	stream.connMu.Unlock()

	stream.active = true
	atomic.AddInt32(&d.activeCount, 1)

	// Inicia sender com retry e ACK reader
	d.startSenderWithRetry(streamIdx)
	d.startACKReader(streamIdx)

	d.logger.Info("parallel stream activated", "stream", streamIdx)
	d.notifyStreamChange()
	return nil
}

// DeactivateStream desativa um stream (para de receber chunks novos).
func (d *Dispatcher) DeactivateStream(streamIdx int) {
	if streamIdx < 0 || streamIdx >= d.maxStreams {
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
	ProducerBps       float64 // bytes/s do produtor
	DrainBps          float64 // bytes/s drenados (soma de todos os streams)
	ProducerBlockedMs int64   // ms que o producer ficou bloqueado (buffer cheio = rede lenta)
	SenderIdleMs      int64   // ms que os senders ficaram ociosos (buffer vazio = producer lento)
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
	producerBlockedNs := atomic.SwapInt64(&d.producerBlockedNs, 0)
	senderIdleNs := atomic.SwapInt64(&d.senderIdleNs, 0)

	var totalDrain int64
	for i := 0; i < d.maxStreams; i++ {
		if d.streams[i].active {
			totalDrain += atomic.SwapInt64(&d.streams[i].drainBytes, 0)
		}
	}

	return RateSample{
		ProducerBps:       float64(producerBytes) / elapsed,
		DrainBps:          float64(totalDrain) / elapsed,
		ProducerBlockedMs: producerBlockedNs / 1e6,
		SenderIdleMs:      senderIdleNs / 1e6,
	}
}

// Close fecha todos os ring buffers e conexões secundárias.
// A conn primária (control) é gerenciada pelo caller (backup.go).
func (d *Dispatcher) Close() {
	for i := 0; i < d.maxStreams; i++ {
		d.streams[i].rb.Close()
		if d.streams[i].conn != nil {
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
// Aceita um context para timeout externo. Se o context expirar,
// fecha todos os ring buffers para desbloquear os senders.
func (d *Dispatcher) WaitAllSenders(ctx context.Context) error {
	done := make(chan error, 1)

	go func() {
		for i := 0; i < d.maxStreams; i++ {
			if d.streams[i].active || d.streams[i].dead {
				if err := d.WaitSender(i); err != nil {
					// Stream morto após esgotar retries — log mas não aborta imediatamente.
					// Outros streams podem ainda estar transmitindo.
					if d.streams[i].dead {
						d.logger.Warn("dead stream sender finished with error",
							"stream", i, "error", err)
						continue
					}
					done <- fmt.Errorf("stream %d sender error: %w", i, err)
					return
				}
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Timeout: fecha todos os ring buffers para desbloquear senders
		d.logger.Warn("WaitAllSenders context expired, closing all ring buffers")
		for i := 0; i < d.maxStreams; i++ {
			d.streams[i].rb.Close()
		}
		return ctx.Err()
	}
}
