// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// mockConn é uma conexão mock que descarta escritas e simula leituras bloqueantes.
type mockConn struct {
	written int64 // atomic
}

func (mc *mockConn) Write(p []byte) (int, error) {
	atomic.AddInt64(&mc.written, int64(len(p)))
	return len(p), nil
}

func (mc *mockConn) Read(p []byte) (int, error) {
	// Simula conn que nunca retorna dados (para ACK reader)
	select {}
}

func (mc *mockConn) Close() error                       { return nil }
func (mc *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (mc *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (mc *mockConn) SetDeadline(t time.Time) error      { return nil }
func (mc *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (mc *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestDispatcher_RoundRobin(t *testing.T) {
	// Cria dispatcher com 3 streams
	conns := make([]*mockConn, 3)
	for i := range conns {
		conns[i] = &mockConn{}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  3,
		BufferSize:  1024 * 1024, // 1MB
		ChunkSize:   1024,        // 1KB chunks
		SessionID:   "test-session",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: conns[0],
	})

	// Ativa streams 1 e 2 manualmente (sem rede)
	d.streams[1].conn = conns[1]
	d.streams[1].active = true
	d.streams[2].conn = conns[2]
	d.streams[2].active = true
	atomic.StoreInt32(&d.activeCount, 3)

	// Escreve 3 chunks de 1KB
	data := make([]byte, 1024)
	for i := 0; i < 3; i++ {
		n, err := d.Write(data)
		if err != nil {
			t.Fatalf("Write chunk %d: %v", i, err)
		}
		if n != 1024 {
			t.Fatalf("expected 1024 bytes written, got %d", n)
		}
	}

	// Verifica que cada ring buffer recebeu 1 chunk (8B header + 1024B data = 1032B)
	for i := 0; i < 3; i++ {
		head := d.streams[i].rb.Head()
		expected := int64(8 + 1024) // ChunkHeader (8B) + data (1024B)
		if head != expected {
			t.Errorf("stream %d: expected head=%d, got %d", i, expected, head)
		}
	}
}

func TestDispatcher_SingleStream(t *testing.T) {
	conn := &mockConn{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  1,
		BufferSize:  1024 * 1024,
		ChunkSize:   512,
		SessionID:   "test-single",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: conn,
	})

	// Escreve 5 chunks — todos devem ir para stream 0
	data := make([]byte, 512)
	for i := 0; i < 5; i++ {
		if _, err := d.Write(data); err != nil {
			t.Fatalf("Write chunk %d: %v", i, err)
		}
	}

	head := d.streams[0].rb.Head()
	expected := int64(5 * (8 + 512)) // 5 chunks × (8B header + 512B data)
	if head != expected {
		t.Errorf("expected head=%d, got %d", expected, head)
	}
}

func TestDispatcher_ActiveStreams(t *testing.T) {
	conn := &mockConn{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  4,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-active",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: conn,
	})

	if d.ActiveStreams() != 1 {
		t.Errorf("expected 1 active stream, got %d", d.ActiveStreams())
	}

	// Ativa stream 1 manualmente
	d.streams[1].conn = &mockConn{}
	d.streams[1].active = true
	atomic.AddInt32(&d.activeCount, 1)

	if d.ActiveStreams() != 2 {
		t.Errorf("expected 2 active streams, got %d", d.ActiveStreams())
	}

	// Desativa stream 1
	d.DeactivateStream(1)
	if d.ActiveStreams() != 1 {
		t.Errorf("expected 1 active stream after deactivate, got %d", d.ActiveStreams())
	}
}

func TestAutoScaler_Hysteresis(t *testing.T) {
	conn := &mockConn{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  4,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-scaler",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: conn,
	})

	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Interval:   100 * time.Millisecond,
		Hysteresis: 3,
		Logger:     logger,
	})

	// Verifica que o scaler não altera nada com 0 métricas
	// (não deve panic/crash)
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	go as.Run(ctx)
	<-ctx.Done()

	// Deve ter mantido apenas 1 stream (sem dados para avaliar)
	if d.ActiveStreams() != 1 {
		t.Errorf("expected 1 active stream (no data), got %d", d.ActiveStreams())
	}
}
