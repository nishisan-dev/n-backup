// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

	// chunkMap mapeia globalSeq → localização no ring buffer para retransmissão via NACK.
	// Consultado apenas no caminho de retransmissão (raro), não impacta performance normal.
	chunkMap   map[uint32]chunkLocation
	chunkMapMu sync.RWMutex
}

// ParallelStream representa um stream individual com seu ring buffer e conexão.
type ParallelStream struct {
	index      uint8
	rb         *RingBuffer
	conn       net.Conn
	connMu     sync.Mutex // protege conn durante reconnect
	writeMu    sync.Mutex // serializa frames no socket (sender + retransmit)
	sendOffset int64      // próximo offset de dados "reais" no ring buffer
	wireOffset int64      // próximo offset observado pelo server (inclui retransmits)
	// ackedRetransmit acumula bytes de retransmissão já ACK'd pelo server.
	// Isso permite traduzir offsets do byte-stream remoto de volta para offsets
	// do ring buffer local, sem precisar materializar retransmits no buffer.
	ackedRetransmit int64
	// retransmitSpans registra retransmits ainda não incorporados em
	// ackedRetransmit. Protegido por sendMu.
	retransmitSpans []retransmitSpan
	sendMu          sync.Mutex
	drainBytes      int64 // atomic — bytes drenados (ACK'd) por este stream
	// senderStarted evita múltiplos sender goroutines para o mesmo stream.
	// Reativação de stream deve reutilizar o sender existente.
	senderStarted atomic.Bool
	active        atomic.Bool
	dead          atomic.Bool // permanentemente morto (esgotou retries)
	senderDone    chan struct{}
	senderErr     chan error
}

type retransmitSpan struct {
	start int64
	end   int64
}

// chunkLocation armazena onde um chunk foi escrito no ring buffer de um stream.
// Usado pelo RetransmitChunk para localizar e reenviar chunks perdidos.
type chunkLocation struct {
	streamIdx int   // índice do stream cujo ring buffer contém o chunk
	rbOffset  int64 // offset absoluto do ChunkHeader no ring buffer
	length    int64 // tamanho total (ChunkHeaderSize + len(data))
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
		chunkMap:       make(map[uint32]chunkLocation),
	}

	// Inicializa todos os streams com ring buffers (inativos)
	for i := 0; i < cfg.MaxStreams; i++ {
		d.streams[i] = &ParallelStream{
			index: uint8(i),
			rb:    NewRingBuffer(cfg.BufferSize),
			// active e dead começam como false (zero value de atomic.Bool)
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
		if s.active.Load() && !s.dead.Load() {
			stream = s
			break
		}
	}
	d.mu.Unlock()

	if stream == nil {
		return ErrAllStreamsDead
	}

	// Escreve ChunkHeader (8 bytes) no ring buffer antes dos dados
	hdr := make([]byte, protocol.ChunkHeaderSize)
	hdr[0] = byte(seq >> 24)
	hdr[1] = byte(seq >> 16)
	hdr[2] = byte(seq >> 8)
	hdr[3] = byte(seq)
	l := uint32(len(data))
	hdr[4] = byte(l >> 24)
	hdr[5] = byte(l >> 16)
	hdr[6] = byte(l >> 8)
	hdr[7] = byte(l)

	// Captura offset antes do write para registrar no chunkMap
	headerOffset := stream.rb.Head()

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

	// Registra localização no chunkMap para suportar retransmissão via NACK
	chunkLen := int64(protocol.ChunkHeaderSize) + int64(len(data))
	d.chunkMapMu.Lock()
	d.chunkMap[seq] = chunkLocation{
		streamIdx: int(stream.index),
		rbOffset:  headerOffset,
		length:    chunkLen,
	}
	d.chunkMapMu.Unlock()

	return nil
}

// RetransmitChunk tenta retransmitir um chunk perdido identificado pelo globalSeq.
// Consulta o chunkMap para localizar o chunk no ring buffer do stream original.
// Se o chunk ainda está no buffer, lê os dados e reenvia pelo MESMO stream que
// contém o chunk no ring buffer. Retransmits não são reanexados ao ring buffer:
// em vez disso, o stream mantém um ledger de bytes retransmitidos para traduzir
// offsets de ChunkSACK/Resume do byte-stream remoto de volta para offsets do
// ring buffer local.
// Retorna (true, nil) se retransmitido com sucesso, (false, nil) se irrecuperável.
func (d *Dispatcher) RetransmitChunk(globalSeq uint32) (bool, error) {
	// Lookup no chunkMap
	d.chunkMapMu.RLock()
	loc, exists := d.chunkMap[globalSeq]
	d.chunkMapMu.RUnlock()

	if !exists {
		d.logger.Warn("retransmit: chunk not found in chunkMap",
			"globalSeq", globalSeq)
		return false, nil
	}

	// Verifica se o chunk ainda está no ring buffer
	stream := d.streams[loc.streamIdx]
	if !stream.rb.ContainsRange(loc.rbOffset, loc.length) {
		d.logger.Error("retransmit: chunk expired from ring buffer",
			"globalSeq", globalSeq,
			"stream", loc.streamIdx,
			"rbOffset", loc.rbOffset,
			"bufferTail", stream.rb.Tail(),
		)
		return false, nil
	}

	// Lê os dados completos (ChunkHeader + payload) do ring buffer
	buf := make([]byte, loc.length)
	n, err := stream.rb.ReadAt(loc.rbOffset, buf)
	if err != nil {
		d.logger.Error("retransmit: failed to read from ring buffer",
			"globalSeq", globalSeq, "error", err)
		return false, fmt.Errorf("reading chunk %d from ring buffer: %w", globalSeq, err)
	}
	if int64(n) < loc.length {
		d.logger.Error("retransmit: short read from ring buffer",
			"globalSeq", globalSeq, "expected", loc.length, "got", n)
		return false, nil
	}

	// Reenvia pelo stream original do chunk para manter a semântica de offsets.
	if stream.dead.Load() {
		return false, fmt.Errorf("retransmit: original stream %d is permanently dead", stream.index)
	}
	if err := d.writeFrame(stream, buf); err != nil {
		d.logger.Warn("retransmit: failed to write to original stream",
			"globalSeq", globalSeq, "stream", stream.index, "error", err)
		return false, fmt.Errorf("retransmitting chunk %d on original stream %d: %w",
			globalSeq, stream.index, err)
	}
	stream.sendMu.Lock()
	stream.recordRetransmitLocked(int64(len(buf)))
	stream.sendMu.Unlock()

	d.logger.Info("retransmit: chunk sent successfully",
		"globalSeq", globalSeq,
		"stream", stream.index,
		"bytes", len(buf),
	)

	return true, nil
}

func (s *ParallelStream) recordRetransmitLocked(length int64) {
	start := s.wireOffset
	s.wireOffset += length
	s.retransmitSpans = append(s.retransmitSpans, retransmitSpan{
		start: start,
		end:   start + length,
	})
}

func (s *ParallelStream) advanceNormalLocked(length int64) {
	s.sendOffset += length
	s.wireOffset += length
}

func (s *ParallelStream) translateWireOffsetLocked(wireOffset int64) int64 {
	baseOffset := wireOffset - s.ackedRetransmit
	for _, span := range s.retransmitSpans {
		if wireOffset <= span.start {
			break
		}
		overlapEnd := minInt64(wireOffset, span.end)
		if overlapEnd > span.start {
			baseOffset -= overlapEnd - span.start
		}
	}
	if baseOffset < 0 {
		return 0
	}
	return baseOffset
}

func (s *ParallelStream) applyACKLocked(wireOffset int64) int64 {
	filtered := s.retransmitSpans[:0]
	for _, span := range s.retransmitSpans {
		switch {
		case span.end <= wireOffset:
			s.ackedRetransmit += span.end - span.start
		case span.start < wireOffset:
			// Nao deveria ocorrer: server so ACKa chunks completos.
			s.ackedRetransmit += wireOffset - span.start
			filtered = append(filtered, retransmitSpan{start: wireOffset, end: span.end})
		default:
			filtered = append(filtered, span)
		}
	}
	s.retransmitSpans = filtered
	return s.translateWireOffsetLocked(wireOffset)
}

func (s *ParallelStream) resumeFromWireOffsetLocked(wireOffset int64) int64 {
	for _, span := range s.retransmitSpans {
		if span.start >= wireOffset {
			continue
		}
		ackedEnd := minInt64(span.end, wireOffset)
		if ackedEnd > span.start {
			s.ackedRetransmit += ackedEnd - span.start
		}
	}
	s.retransmitSpans = nil
	s.wireOffset = wireOffset
	s.sendOffset = wireOffset - s.ackedRetransmit
	if s.sendOffset < 0 {
		s.sendOffset = 0
	}
	return s.sendOffset
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (d *Dispatcher) readChunkFrame(stream *ParallelStream, offset int64) ([]byte, error) {
	hdr := make([]byte, protocol.ChunkHeaderSize)
	n, err := stream.rb.ReadFullAt(offset, hdr)
	if err != nil {
		return nil, err
	}
	if n < protocol.ChunkHeaderSize {
		return nil, io.ErrUnexpectedEOF
	}

	length := binary.BigEndian.Uint32(hdr[4:8])
	if length > uint32(d.chunkSize) {
		return nil, fmt.Errorf("invalid chunk length %d at offset %d", length, offset)
	}

	frame := make([]byte, protocol.ChunkHeaderSize+int(length))
	copy(frame, hdr)
	if length == 0 {
		return frame, nil
	}

	n, err = stream.rb.ReadFullAt(offset+protocol.ChunkHeaderSize, frame[protocol.ChunkHeaderSize:])
	if err != nil {
		return nil, err
	}
	if n < int(length) {
		return nil, io.ErrUnexpectedEOF
	}

	return frame, nil
}

func (d *Dispatcher) writeFrame(stream *ParallelStream, frame []byte) error {
	stream.writeMu.Lock()
	defer stream.writeMu.Unlock()

	stream.connMu.Lock()
	conn := stream.conn
	stream.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("stream %d has no connection", stream.index)
	}

	if netConn, ok := conn.(net.Conn); ok {
		netConn.SetWriteDeadline(time.Now().Add(writeDeadline))
	}

	written := 0
	for written < len(frame) {
		n, err := conn.Write(frame[written:])
		if n > 0 {
			written += n
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}

	return nil
}

// startSenderWithRetry inicia a goroutine sender para um stream com retry automático.
// Na falha de conn.Write, tenta reconectar via ParallelJoin com backoff exponencial.
// O server retorna lastOffset no ParallelACK, permitindo retomar da posição correta.
func (d *Dispatcher) startSenderWithRetry(streamIdx int) {
	stream := d.streams[streamIdx]
	if !stream.senderStarted.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer close(stream.senderDone)
		defer func() {
			stream.connMu.Lock()
			if stream.conn != nil {
				stream.conn.Close()
			}
			stream.connMu.Unlock()
		}()
		retries := 0

		for {
			stream.sendMu.Lock()
			offset := stream.sendOffset
			stream.sendMu.Unlock()

			// Diagnóstico: mede tempo bloqueado em rb.ReadFullAt (indica buffer vazio = producer lento)
			readStart := time.Now()
			frame, err := d.readChunkFrame(stream, offset)
			if elapsed := time.Since(readStart); elapsed > time.Millisecond {
				atomic.AddInt64(&d.senderIdleNs, elapsed.Nanoseconds())
			}
			if err != nil {
				if err == ErrBufferClosed {
					stream.senderErr <- nil
					return
				}
				if err == ErrOffsetExpired {
					d.logger.Error("ring buffer offset expired, data already freed — stream irrecoverable",
						"stream", streamIdx, "offset", offset,
						"rbTail", stream.rb.Tail(), "rbHead", stream.rb.Head())
					stream.dead.Store(true)
					d.DeactivateStream(streamIdx)
					stream.senderErr <- fmt.Errorf("stream %d: offset expired (data freed from ring buffer)", streamIdx)
					return
				}
				stream.senderErr <- err
				return
			}

			// Escreve um frame completo por vez para não quebrar o framing quando
			// uma retransmissão precisar injetar um chunk no mesmo stream.
			writeErr := d.writeFrame(stream, frame)

			if writeErr != nil {
				d.logger.Warn("stream write failed, attempting reconnect",
					"stream", streamIdx, "error", writeErr, "retry", retries+1)

				// Tenta reconectar com backoff
				if retries >= maxRetriesPerStream {
					d.logger.Error("stream permanently dead, max retries exceeded",
						"stream", streamIdx, "retries", retries)
					stream.dead.Store(true)
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
				// Valida que o RingBuffer ainda contém os dados no resumeOffset
				if resumeOffset > 0 && !stream.rb.ContainsRange(resumeOffset, protocol.ChunkHeaderSize) {
					// Se resumeOffset == head, todos os dados até aqui já foram ACK'd
					// pelo server. O stream está sincronizado e pode continuar
					// enviando novos dados a partir do head — não é irrecuperável.
					if resumeOffset == stream.rb.Head() {
						d.logger.Info("resume offset equals head — all data ACK'd, continuing from head",
							"stream", streamIdx, "resumeOffset", resumeOffset)
					} else {
						d.logger.Error("resume offset no longer in ring buffer — stream irrecoverable",
							"stream", streamIdx, "resumeOffset", resumeOffset,
							"rbTail", stream.rb.Tail(), "rbHead", stream.rb.Head())
						stream.dead.Store(true)
						d.DeactivateStream(streamIdx)
						stream.senderErr <- fmt.Errorf("stream %d: resume offset %d expired from ring buffer", streamIdx, resumeOffset)
						return
					}
				}

				// Valida alinhamento: os primeiros bytes no resumeOffset devem
				// formar um ChunkHeader válido (GlobalSeq + Length <= chunkSize).
				// Se não, houve dessincronização — dados corrompidos.
				// Pula validação quando resumeOffset == head (buffer vazio nessa posição).
				if resumeOffset > 0 && resumeOffset < stream.rb.Head() {
					hdrBuf := make([]byte, protocol.ChunkHeaderSize)
					n, readErr := stream.rb.ReadAt(resumeOffset, hdrBuf)
					if readErr != nil || n < protocol.ChunkHeaderSize {
						d.logger.Error("failed to read chunk header at resume offset",
							"stream", streamIdx, "offset", resumeOffset, "error", readErr)
						stream.dead.Store(true)
						d.DeactivateStream(streamIdx)
						stream.senderErr <- fmt.Errorf("stream %d: cannot read header at resume offset %d", streamIdx, resumeOffset)
						return
					}
					hdrLength := binary.BigEndian.Uint32(hdrBuf[4:8])
					if hdrLength > uint32(d.chunkSize) {
						d.logger.Error("chunk header at resume offset has invalid length — desync detected",
							"stream", streamIdx, "offset", resumeOffset,
							"hdrLength", hdrLength, "maxChunkSize", d.chunkSize)
						stream.dead.Store(true)
						d.DeactivateStream(streamIdx)
						stream.senderErr <- fmt.Errorf("stream %d: desync at resume offset %d (invalid chunk length %d)",
							streamIdx, resumeOffset, hdrLength)
						return
					}
				}

				stream.sendMu.Lock()
				stream.resumeFromWireOffsetLocked(resumeOffset)
				stream.sendMu.Unlock()

				d.logger.Info("stream reconnected, resuming from offset",
					"stream", streamIdx, "offset", resumeOffset,
					"resumeSendOffset", stream.sendOffset,
					"rbTail", stream.rb.Tail(), "rbHead", stream.rb.Head())
				retries = 0 // reset após reconexão bem-sucedida — evita morte permanente por acúmulo
				continue
			}

			// Write bem-sucedido — reset retries e avança offset
			retries = 0
			stream.sendMu.Lock()
			stream.advanceNormalLocked(int64(len(frame)))
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

	// Envia ParallelJoin com medição de RTT
	joinStart := time.Now()
	if err := protocol.WriteParallelJoin(tlsConn, d.sessionID, uint8(streamIdx)); err != nil {
		tlsConn.Close()
		return 0, fmt.Errorf("writing ParallelJoin stream %d: %w", streamIdx, err)
	}

	// Lê ParallelACK com lastOffset
	ack, err := protocol.ReadParallelACK(tlsConn)
	reconnectRTT := time.Since(joinStart)
	if err != nil {
		tlsConn.Close()
		return 0, fmt.Errorf("reading ParallelACK stream %d: %w", streamIdx, err)
	}

	d.logger.Info("reconnect ACK received", "stream", streamIdx, "reconnect_rtt", reconnectRTT)

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
		lastBaseOffset := stream.rb.Tail()
		for {
			stream.connMu.Lock()
			conn := stream.conn
			stream.connMu.Unlock()

			csack, err := protocol.ReadChunkSACK(conn)
			if err != nil {
				return // conn fechada ou erro — sender tratará a reconexão
			}

			newWireOffset := int64(csack.Offset)
			stream.sendMu.Lock()
			newBaseOffset := stream.applyACKLocked(newWireOffset)
			stream.sendMu.Unlock()
			stream.rb.Advance(newBaseOffset)

			// Acumula apenas o delta (bytes novos drenados desde o último SACK)
			delta := newBaseOffset - lastBaseOffset
			if delta > 0 {
				atomic.AddInt64(&stream.drainBytes, delta)
			}
			lastBaseOffset = newBaseOffset

			d.logger.Debug("ChunkSACK received",
				"stream", streamIdx,
				"chunkSeq", csack.ChunkSeq,
				"wireOffset", csack.Offset,
				"baseOffset", newBaseOffset,
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
	if stream.dead.Load() {
		return fmt.Errorf("stream %d is permanently dead", streamIdx)
	}
	if stream.active.Load() {
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

	// Envia ParallelJoin com medição de RTT
	joinStart := time.Now()
	if err := protocol.WriteParallelJoin(tlsConn, d.sessionID, uint8(streamIdx)); err != nil {
		tlsConn.Close()
		return fmt.Errorf("writing ParallelJoin stream %d: %w", streamIdx, err)
	}

	// Lê ParallelACK
	ack, err := protocol.ReadParallelACK(tlsConn)
	joinRTT := time.Since(joinStart)
	if err != nil {
		tlsConn.Close()
		return fmt.Errorf("reading ParallelACK stream %d: %w", streamIdx, err)
	}

	d.logger.Info("parallel join ACK received", "stream", streamIdx, "parallel_join_rtt", joinRTT)

	if ack.Status != protocol.ParallelStatusOK {
		tlsConn.Close()
		return fmt.Errorf("server rejected ParallelJoin stream %d: status=%d", streamIdx, ack.Status)
	}

	stream.connMu.Lock()
	stream.conn = tlsConn
	stream.connMu.Unlock()

	stream.active.Store(true)
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
	if !stream.active.Load() {
		return
	}

	stream.active.Store(false)
	atomic.AddInt32(&d.activeCount, -1)
	d.logger.Info("parallel stream deactivated", "stream", streamIdx)
	d.notifyStreamChange()
}

// ActiveStreams retorna o número de streams ativos.
func (d *Dispatcher) ActiveStreams() int {
	return int(atomic.LoadInt32(&d.activeCount))
}

// NextActivatableStream retorna o primeiro índice livre que ainda não foi marcado
// como permanentemente morto. Retorna -1 se não houver candidatos.
func (d *Dispatcher) NextActivatableStream() int {
	for i := 0; i < d.maxStreams; i++ {
		stream := d.streams[i]
		if stream.active.Load() || stream.dead.Load() {
			continue
		}
		return i
	}
	return -1
}

// LastActiveStream retorna o maior índice atualmente ativo. Retorna -1 se não houver.
func (d *Dispatcher) LastActiveStream() int {
	for i := d.maxStreams - 1; i >= 0; i-- {
		if d.streams[i].active.Load() {
			return i
		}
	}
	return -1
}

// DrainStream prepara um stream para flow rotation graceful.
// Chamado pelo control channel quando o server envia ControlRotate(idx).
// O fluxo é: server envia ControlRotate → agent DrainStream → agent envia ACK →
// server fecha data conn → sender detecta e reconecta automaticamente.
// Atualmente apenas loga a intenção — o sender com retry+resume cuida da reconexão.
func (d *Dispatcher) DrainStream(streamIdx uint8) {
	if int(streamIdx) >= d.maxStreams {
		return
	}

	stream := d.streams[streamIdx]
	if !stream.active.Load() {
		d.logger.Info("drain requested for inactive stream, ignoring",
			"stream", streamIdx)
		return
	}

	d.logger.Info("stream drain requested for graceful rotation",
		"stream", streamIdx,
		"sendOffset", atomic.LoadInt64(&stream.drainBytes),
	)
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
		if d.streams[i].active.Load() {
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
		// A conexão não é fechada aqui para permitir que as sender goroutines
		// drenem o restante do buffer para a rede. A goroutine fechará a conexão via defer.
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
		var deadErr error
		for i := 0; i < d.maxStreams; i++ {
			if d.streams[i].active.Load() || d.streams[i].dead.Load() {
				if err := d.WaitSender(i); err != nil {
					// Stream morto após esgotar retries — chunks no ring buffer foram perdidos.
					// Acumula o primeiro erro para retorno, mas espera todos os senders.
					if d.streams[i].dead.Load() {
						d.logger.Warn("dead stream sender finished with error",
							"stream", i, "error", err)
						if deadErr == nil {
							deadErr = fmt.Errorf("stream %d died with unsent data: %w", i, err)
						}
						continue
					}
					done <- fmt.Errorf("stream %d sender error: %w", i, err)
					return
				}
			}
		}
		done <- deadErr
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
