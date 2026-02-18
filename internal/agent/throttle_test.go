// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestThrottledWriter_ZeroBypasses(t *testing.T) {
	var buf bytes.Buffer
	w := NewThrottledWriter(context.Background(), &buf, 0)

	// Quando bandwidthLimit=0, deve retornar o writer original (sem wrapper)
	if _, ok := w.(*ThrottledWriter); ok {
		t.Fatal("expected original writer (bypass), got ThrottledWriter")
	}

	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected %d bytes written, got %d", len(data), n)
	}
	if buf.String() != "hello world" {
		t.Errorf("expected 'hello world', got %q", buf.String())
	}
}

func TestThrottledWriter_SmallWrites(t *testing.T) {
	var buf bytes.Buffer
	// 1 MB/s — escritas pequenas devem funcionar sem bloquear significativamente
	w := NewThrottledWriter(context.Background(), &buf, 1*1024*1024)

	data := []byte("small")
	for i := 0; i < 10; i++ {
		_, err := w.Write(data)
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	if buf.Len() != 50 {
		t.Errorf("expected 50 bytes written, got %d", buf.Len())
	}
}

func TestThrottledWriter_RespectsBandwidthLimit(t *testing.T) {
	var buf bytes.Buffer

	// Limite: 100 KB/s — burst é min(100KB, maxBurstSize=256KB) = 100KB
	// Escrevemos 400 KB: burst cobre ~100KB, restante ~300KB a 100KB/s = ~3s mínimo
	limit := int64(100 * 1024) // 100 KB/s
	w := NewThrottledWriter(context.Background(), &buf, limit)

	data := make([]byte, 400*1024) // 400 KB
	for i := range data {
		data[i] = byte(i % 256)
	}

	start := time.Now()
	n, err := w.Write(data)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected %d bytes written, got %d", len(data), n)
	}

	// 400KB total: burst cobre 100KB, restante 300KB a 100KB/s = ~3s
	// Margem inferior de 2s para tolerância de CI
	minExpected := 2 * time.Second
	if elapsed < minExpected {
		t.Errorf("throttle too fast: wrote %d bytes in %v (limit=%d B/s, expected >= %v)",
			len(data), elapsed, limit, minExpected)
	}

	// Margem superior generosa para CI lento
	maxExpected := 8 * time.Second
	if elapsed > maxExpected {
		t.Errorf("throttle too slow: wrote %d bytes in %v (limit=%d B/s, expected <= %v)",
			len(data), elapsed, limit, maxExpected)
	}
}

func TestThrottledWriter_ContextCancellation(t *testing.T) {
	var buf bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	w := NewThrottledWriter(ctx, &buf, 1024) // 1 KB/s — muito lento

	// Cancela o contexto enquanto escreve dados grandes
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	data := make([]byte, 100*1024) // 100 KB @ 1 KB/s = ~100s sem cancel
	_, err := w.Write(data)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestThrottledWriter_NegativeBypasses(t *testing.T) {
	var buf bytes.Buffer
	w := NewThrottledWriter(context.Background(), &buf, -1)

	// Quando bandwidthLimit<0, deve retornar o writer original (sem wrapper)
	if _, ok := w.(*ThrottledWriter); ok {
		t.Fatal("expected original writer (bypass), got ThrottledWriter")
	}
}
