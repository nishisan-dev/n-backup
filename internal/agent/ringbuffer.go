// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"errors"
	"sync"
)

// Erros do RingBuffer.
var (
	ErrBufferClosed   = errors.New("ringbuffer: closed")
	ErrOffsetExpired  = errors.New("ringbuffer: offset no longer in buffer")
	ErrOffsetNotReady = errors.New("ringbuffer: offset not yet written")
)

// RingBuffer é um buffer circular thread-safe com backpressure.
// O produtor escreve via Write() (bloqueia quando cheio).
// O consumidor lê via ReadAt() a partir de offsets absolutos.
// O server ACK avança o tail via Advance(), liberando espaço.
type RingBuffer struct {
	buf  []byte
	size int64

	// Offsets absolutos no stream (nunca resetam)
	head int64 // próxima posição de escrita
	tail int64 // posição mais antiga ainda no buffer (avançada por Advance)

	closed bool
	mu     sync.Mutex
	// notFull sinaliza que há espaço para escrita
	notFull sync.Cond
	// notEmpty sinaliza que há dados para leitura
	notEmpty sync.Cond
}

// NewRingBuffer cria um ring buffer com o tamanho especificado em bytes.
func NewRingBuffer(size int64) *RingBuffer {
	rb := &RingBuffer{
		buf:  make([]byte, size),
		size: size,
	}
	rb.notFull.L = &rb.mu
	rb.notEmpty.L = &rb.mu
	return rb
}

// Write implementa io.Writer. Bloqueia quando o buffer está cheio (backpressure).
// Retorna ErrBufferClosed se o buffer foi fechado.
func (rb *RingBuffer) Write(p []byte) (int, error) {
	written := 0

	for written < len(p) {
		rb.mu.Lock()

		// Espera até ter espaço ou ser fechado
		for rb.available() == 0 && !rb.closed {
			rb.notFull.Wait()
		}

		if rb.closed {
			rb.mu.Unlock()
			return written, ErrBufferClosed
		}

		// Calcula quanto pode escrever
		avail := rb.available()
		chunk := len(p) - written
		if int64(chunk) > avail {
			chunk = int(avail)
		}

		// Escreve no buffer circular
		start := rb.head % rb.size
		if start+int64(chunk) <= rb.size {
			// Cabe sem wrap
			copy(rb.buf[start:], p[written:written+chunk])
		} else {
			// Precisa wrap
			firstPart := int(rb.size - start)
			copy(rb.buf[start:], p[written:written+firstPart])
			copy(rb.buf[0:], p[written+firstPart:written+chunk])
		}

		rb.head += int64(chunk)
		written += chunk

		rb.notEmpty.Broadcast()
		rb.mu.Unlock()
	}

	return written, nil
}

// ReadAt lê até len(p) bytes a partir do offset absoluto no stream.
// Retorna o número de bytes lidos.
// Retorna ErrOffsetExpired se o offset já foi descartado (Advance passou dele).
// Bloqueia se o offset ainda não foi escrito (aguarda produtor).
func (rb *RingBuffer) ReadAt(offset int64, p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Verifica se o offset já foi descartado
	if offset < rb.tail {
		return 0, ErrOffsetExpired
	}

	// Espera até ter dados no offset ou fechar
	for offset >= rb.head && !rb.closed {
		rb.notEmpty.Wait()
	}

	// Se fechou e não tem dados no offset
	if offset >= rb.head {
		return 0, ErrBufferClosed
	}

	// Calcula quanto pode ler
	readable := int(rb.head - offset)
	if readable > len(p) {
		readable = len(p)
	}

	// Lê do buffer circular
	start := offset % rb.size
	if start+int64(readable) <= rb.size {
		copy(p, rb.buf[start:start+int64(readable)])
	} else {
		firstPart := int(rb.size - start)
		copy(p, rb.buf[start:])
		copy(p[firstPart:], rb.buf[0:readable-firstPart])
	}

	return readable, nil
}

// Advance move o tail para o offset especificado, liberando espaço no buffer.
// Chamado quando o server confirma recebimento (SACK).
func (rb *RingBuffer) Advance(offset int64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if offset > rb.tail {
		if offset > rb.head {
			offset = rb.head // não avança além do que foi escrito
		}
		rb.tail = offset
		rb.notFull.Broadcast()
	}
}

// Contains verifica se o offset absoluto ainda está presente no buffer.
func (rb *RingBuffer) Contains(offset int64) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	return offset >= rb.tail && offset < rb.head
}

// ContainsRange verifica se uma faixa completa [offset, offset+length) está no buffer.
// Retorna true se todos os bytes da faixa estão disponíveis para leitura.
func (rb *RingBuffer) ContainsRange(offset, length int64) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	return offset >= rb.tail && (offset+length) <= rb.head
}

// Head retorna o offset absoluto da próxima posição de escrita.
func (rb *RingBuffer) Head() int64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.head
}

// Tail retorna o offset absoluto da posição mais antiga no buffer.
func (rb *RingBuffer) Tail() int64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.tail
}

// Close fecha o buffer. Write retorna erro, ReadAt retorna dados restantes.
func (rb *RingBuffer) Close() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.closed = true
	rb.notFull.Broadcast()
	rb.notEmpty.Broadcast()
}

// available retorna bytes disponíveis para escrita.
// Deve ser chamada com rb.mu held.
func (rb *RingBuffer) available() int64 {
	used := rb.head - rb.tail
	return rb.size - used
}
