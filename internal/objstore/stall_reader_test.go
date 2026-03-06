// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package objstore

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestStallDetectReader_NormalRead(t *testing.T) {
	data := []byte("hello world backup data 1234567890")
	inner := bytes.NewReader(data)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := newStallDetectReader(inner, 500*time.Millisecond, cancel)
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("data mismatch: got %q, want %q", got, data)
	}

	// Contexto não deve ter sido cancelado
	select {
	case <-ctx.Done():
		t.Error("context should not be cancelled after successful read")
	default:
	}
}

func TestStallDetectReader_StallCancelsContext(t *testing.T) {
	// slowReader simula um reader que trava após o primeiro byte
	sr := &slowReader{
		data:       []byte("AB"),
		stallAfter: 1,
		stallFor:   2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stallTimeout := 200 * time.Millisecond
	r := newStallDetectReader(sr, stallTimeout, cancel)
	defer r.Close()

	buf := make([]byte, 2)
	// Primeiro read funciona (1 byte)
	n, err := r.Read(buf[:1])
	if err != nil || n != 1 {
		t.Fatalf("first read: n=%d, err=%v", n, err)
	}

	// Segundo read trava — stall detection deve cancelar o contexto
	// antes do slowReader retornar
	time.Sleep(stallTimeout + 200*time.Millisecond)

	select {
	case <-ctx.Done():
		// esperado — stall detection acionou cancel
	default:
		t.Error("context should have been cancelled due to stall")
	}
}

func TestStallDetectReader_CloseStopsTimer(t *testing.T) {
	inner := bytes.NewReader([]byte("test"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := newStallDetectReader(inner, 100*time.Millisecond, cancel)
	r.Close()

	// Mesmo após o stallTimeout, contexto não deve ser cancelado
	time.Sleep(250 * time.Millisecond)

	select {
	case <-ctx.Done():
		t.Error("context should not be cancelled after Close()")
	default:
	}
}

func TestStallDetectReader_DefaultTimeout(t *testing.T) {
	inner := bytes.NewReader([]byte("test"))
	cancel := func() {}

	r := newStallDetectReader(inner, 0, cancel)
	defer r.Close()

	if r.stallTimeout != defaultStallTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultStallTimeout, r.stallTimeout)
	}
}

// slowReader é um io.Reader que trava após N bytes lidos.
type slowReader struct {
	data       []byte
	pos        int
	stallAfter int
	stallFor   time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.pos >= r.stallAfter {
		time.Sleep(r.stallFor)
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
