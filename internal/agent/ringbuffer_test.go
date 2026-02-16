// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestRingBuffer_WriteRead(t *testing.T) {
	rb := NewRingBuffer(1024)

	data := []byte("hello world")
	n, err := rb.Write(data)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}

	buf := make([]byte, 1024)
	n, err = rb.ReadAt(0, buf)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if !bytes.Equal(buf[:n], data) {
		t.Fatalf("expected %q, got %q", data, buf[:n])
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	rb := NewRingBuffer(16)

	// Escreve 10 bytes
	data1 := []byte("0123456789")
	rb.Write(data1)

	// Avança tail para liberar espaço
	rb.Advance(10)

	// Escreve mais 10 bytes (wrap around)
	data2 := []byte("ABCDEFGHIJ")
	rb.Write(data2)

	// Lê os dados do wrap
	buf := make([]byte, 10)
	n, err := rb.ReadAt(10, buf)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if !bytes.Equal(buf[:n], data2) {
		t.Fatalf("expected %q, got %q", data2, buf[:n])
	}
}

func TestRingBuffer_Backpressure(t *testing.T) {
	rb := NewRingBuffer(64)

	// Enche o buffer
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	rb.Write(data)

	// Write deve bloquear até Advance ser chamado
	done := make(chan struct{})
	go func() {
		rb.Write([]byte("extra"))
		close(done)
	}()

	// Verifica que ainda está bloqueado após 100ms
	select {
	case <-done:
		t.Fatal("Write should have blocked")
	case <-time.After(100 * time.Millisecond):
		// OK, está bloqueado
	}

	// Libera espaço
	rb.Advance(5)

	// Agora deve desbloquear
	select {
	case <-done:
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("Write should have unblocked after Advance")
	}
}

func TestRingBuffer_Advance(t *testing.T) {
	rb := NewRingBuffer(256)

	rb.Write([]byte("hello"))
	rb.Advance(3)

	if rb.Tail() != 3 {
		t.Fatalf("expected tail=3, got %d", rb.Tail())
	}

	// Offset 0 não está mais no buffer
	if rb.Contains(0) {
		t.Fatal("offset 0 should not be in buffer after Advance(3)")
	}

	// Offset 3 ainda está
	if !rb.Contains(3) {
		t.Fatal("offset 3 should be in buffer")
	}
}

func TestRingBuffer_OffsetExpired(t *testing.T) {
	rb := NewRingBuffer(256)

	rb.Write([]byte("hello"))
	rb.Advance(5)

	buf := make([]byte, 5)
	_, err := rb.ReadAt(0, buf)
	if err != ErrOffsetExpired {
		t.Fatalf("expected ErrOffsetExpired, got %v", err)
	}
}

func TestRingBuffer_Close(t *testing.T) {
	rb := NewRingBuffer(64)

	// Fecha o buffer
	rb.Close()

	// Write deve retornar erro
	_, err := rb.Write([]byte("hello"))
	if err != ErrBufferClosed {
		t.Fatalf("expected ErrBufferClosed, got %v", err)
	}
}

func TestRingBuffer_CloseUnblocksWrite(t *testing.T) {
	rb := NewRingBuffer(8)

	// Enche o buffer
	rb.Write(make([]byte, 8))

	done := make(chan error)
	go func() {
		_, err := rb.Write([]byte("more"))
		done <- err
	}()

	// Fecha o buffer pra desbloquear o Write
	time.Sleep(50 * time.Millisecond)
	rb.Close()

	select {
	case err := <-done:
		if err != ErrBufferClosed {
			t.Fatalf("expected ErrBufferClosed, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Close should unblock Write")
	}
}

func TestRingBuffer_ReadAtWaitsForData(t *testing.T) {
	rb := NewRingBuffer(256)

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 5)
		rb.ReadAt(0, buf)
		close(done)
	}()

	// ReadAt deve bloquear esperando dados
	select {
	case <-done:
		t.Fatal("ReadAt should block when no data")
	case <-time.After(100 * time.Millisecond):
	}

	// Escreve dados
	rb.Write([]byte("hello"))

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("ReadAt should unblock after Write")
	}
}

func TestRingBuffer_ConcurrentWriteRead(t *testing.T) {
	rb := NewRingBuffer(4096)
	totalBytes := int64(4096 * 50) // 200KB no total

	var wg sync.WaitGroup

	// Produtor
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunk := make([]byte, 256)
		for i := range chunk {
			chunk[i] = byte(i % 256)
		}
		for written := int64(0); written < totalBytes; written += int64(len(chunk)) {
			if _, err := rb.Write(chunk); err != nil {
				t.Errorf("Write error: %v", err)
				return
			}
		}
		rb.Close()
	}()

	// Consumidor + ACK
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 512)
		offset := int64(0)
		for offset < totalBytes {
			n, err := rb.ReadAt(offset, buf)
			if err != nil {
				if err == ErrBufferClosed {
					return
				}
				t.Errorf("ReadAt(%d) error: %v", offset, err)
				return
			}
			offset += int64(n)
			// ACK imediato para liberar espaço (simula consumer rápido)
			rb.Advance(offset)
		}
	}()

	wg.Wait()
}

func TestRingBuffer_AdvanceBeyondHead(t *testing.T) {
	rb := NewRingBuffer(256)

	rb.Write([]byte("hello"))

	// Advance além do head não deve ir além
	rb.Advance(9999)

	if rb.Tail() != rb.Head() {
		t.Fatalf("Advance beyond head: tail=%d, head=%d", rb.Tail(), rb.Head())
	}
}

func TestRingBuffer_HeadTail(t *testing.T) {
	rb := NewRingBuffer(256)

	if rb.Head() != 0 || rb.Tail() != 0 {
		t.Fatalf("initial: head=%d tail=%d (expected 0,0)", rb.Head(), rb.Tail())
	}

	rb.Write([]byte("12345"))
	if rb.Head() != 5 {
		t.Fatalf("after write: head=%d (expected 5)", rb.Head())
	}

	rb.Advance(3)
	if rb.Tail() != 3 {
		t.Fatalf("after advance: tail=%d (expected 3)", rb.Tail())
	}
}

func TestRingBuffer_ContainsRange(t *testing.T) {
	rb := NewRingBuffer(256)

	rb.Write([]byte("0123456789")) // head=10, tail=0

	// Faixa completa disponível
	if !rb.ContainsRange(0, 10) {
		t.Fatal("expected ContainsRange(0,10) = true")
	}

	// Faixa parcial disponível
	if !rb.ContainsRange(5, 5) {
		t.Fatal("expected ContainsRange(5,5) = true")
	}

	// Faixa além do head
	if rb.ContainsRange(5, 10) {
		t.Fatal("expected ContainsRange(5,10) = false (exceeds head)")
	}

	// Avança tail — dados antigos liberados
	rb.Advance(6)

	// Faixa que começa antes do tail
	if rb.ContainsRange(3, 4) {
		t.Fatal("expected ContainsRange(3,4) = false (before tail)")
	}

	// Faixa dentro do intervalo válido
	if !rb.ContainsRange(6, 4) {
		t.Fatal("expected ContainsRange(6,4) = true")
	}

	// Faixa de tamanho zero (edge case)
	if !rb.ContainsRange(6, 0) {
		t.Fatal("expected ContainsRange(6,0) = true (zero length)")
	}
}
