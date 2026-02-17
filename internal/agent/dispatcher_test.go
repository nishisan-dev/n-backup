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

	"github.com/nishisan-dev/n-backup/internal/protocol"
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

// activateStreamManually ativa um stream sem rede (para testes unitários).
func activateStreamManually(d *Dispatcher, idx int, conn net.Conn) {
	s := d.streams[idx]
	s.conn = conn
	s.active.Store(true)
	atomic.AddInt32(&d.activeCount, 1)
}

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
		PrimaryConn: nil, // control-only, não usado nos testes de round-robin
	})

	// Ativa todos os streams manualmente (sem rede)
	for i := 0; i < 3; i++ {
		activateStreamManually(d, i, conns[i])
	}

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
		PrimaryConn: nil,
	})

	// Ativa stream 0 manualmente
	activateStreamManually(d, 0, conn)

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
		PrimaryConn: nil,
	})

	// Nenhum stream ativo inicialmente
	if d.ActiveStreams() != 0 {
		t.Errorf("expected 0 active streams, got %d", d.ActiveStreams())
	}

	// Ativa stream 0 manualmente
	activateStreamManually(d, 0, &mockConn{})
	if d.ActiveStreams() != 1 {
		t.Errorf("expected 1 active stream, got %d", d.ActiveStreams())
	}

	// Ativa stream 1
	activateStreamManually(d, 1, &mockConn{})
	if d.ActiveStreams() != 2 {
		t.Errorf("expected 2 active streams, got %d", d.ActiveStreams())
	}

	// Desativa stream 1
	d.DeactivateStream(1)
	if d.ActiveStreams() != 1 {
		t.Errorf("expected 1 active stream after deactivate, got %d", d.ActiveStreams())
	}
}

func TestDispatcher_AllDeadStreams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  2,
		BufferSize:  1024 * 1024,
		ChunkSize:   512,
		SessionID:   "test-dead",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	// Ativa e marca como mortos
	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})
	d.streams[0].dead.Store(true)
	d.streams[1].dead.Store(true)

	// Write deve retornar ErrAllStreamsDead
	data := make([]byte, 512)
	_, err := d.Write(data)
	if err != ErrAllStreamsDead {
		t.Errorf("expected ErrAllStreamsDead, got %v", err)
	}
}

func TestDispatcher_SkipDeadStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  3,
		BufferSize:  1024 * 1024,
		ChunkSize:   512,
		SessionID:   "test-skip-dead",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	// Ativa streams 0, 1, 2
	for i := 0; i < 3; i++ {
		activateStreamManually(d, i, &mockConn{})
	}

	// Marca stream 1 como morto
	d.streams[1].dead.Store(true)

	// Escreve 4 chunks — devem ir para streams 0 e 2 (skip 1)
	data := make([]byte, 512)
	for i := 0; i < 4; i++ {
		if _, err := d.Write(data); err != nil {
			t.Fatalf("Write chunk %d: %v", i, err)
		}
	}

	// Stream 0 e 2 devem ter recebido chunks, stream 1 nenhum
	if d.streams[1].rb.Head() != 0 {
		t.Errorf("dead stream 1 should have head=0, got %d", d.streams[1].rb.Head())
	}
	if d.streams[0].rb.Head() == 0 {
		t.Error("stream 0 should have received chunks")
	}
	if d.streams[2].rb.Head() == 0 {
		t.Error("stream 2 should have received chunks")
	}
}

func TestDispatcher_WaitAllSendersContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  1,
		BufferSize:  1024 * 1024,
		ChunkSize:   512,
		SessionID:   "test-wait-ctx",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	// Ativa stream ma não inicia sender — senderDone nunca fecha
	activateStreamManually(d, 0, &mockConn{})

	// WaitAllSenders com context que expira rapidamente
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := d.WaitAllSenders(ctx)
	if err == nil {
		t.Fatal("expected context timeout error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestAutoScaler_Hysteresis(t *testing.T) {
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
		PrimaryConn: nil,
	})

	// Ativa stream 0 para o scaler ter algo com que trabalhar
	activateStreamManually(d, 0, &mockConn{})

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

func TestDispatcher_StartSenderWithRetry_Idempotent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  1,
		BufferSize:  1024 * 1024,
		ChunkSize:   512,
		SessionID:   "test-idempotent-sender",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	activateStreamManually(d, 0, &mockConn{})
	stream := d.streams[0]

	// Chamar duas vezes não pode criar dois senders.
	d.startSenderWithRetry(0)
	d.startSenderWithRetry(0)

	if !stream.senderStarted.Load() {
		t.Fatal("expected senderStarted=true after first start")
	}

	// Fechar o buffer força término do sender.
	stream.rb.Close()

	select {
	case <-stream.senderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sender did not stop after ring buffer close")
	}
}

func TestAutoScaler_Adaptive_ProbeSuccessKeepsStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  4,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-adaptive-success",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})

	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Hysteresis: 3,
		Logger:     logger,
		Mode:       "adaptive",
	})

	as.probeState = probeProbing
	as.probeBaseline = 100
	as.probeWindows = probeWindowsRequired - 1

	rates := RateSample{
		ProducerBps: 60,
		DrainBps:    50, // total=110 => +10%
	}

	as.evaluateAdaptive(1.0, rates, d.ActiveStreams())

	if as.probeState != probeIdle {
		t.Fatalf("expected probeState=probeIdle, got %d", as.probeState)
	}
	if as.probeBaseline != 0 {
		t.Fatalf("expected probeBaseline reset to 0, got %f", as.probeBaseline)
	}
	if d.ActiveStreams() != 2 {
		t.Fatalf("expected 2 active streams after successful probe, got %d", d.ActiveStreams())
	}

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateScalingUp {
		t.Fatalf("expected snapshot state ScalingUp, got %d", snap.State)
	}
	if snap.ProbeActive {
		t.Fatal("expected probe_active=false after successful probe")
	}
}

func TestAutoScaler_Adaptive_ProbeFailRevertsStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  4,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-adaptive-fail",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})

	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Hysteresis: 3,
		Logger:     logger,
		Mode:       "adaptive",
	})

	as.probeState = probeProbing
	as.probeBaseline = 100
	as.probeWindows = probeWindowsRequired - 1

	rates := RateSample{
		ProducerBps: 50,
		DrainBps:    50, // total=100 => 0% gain
	}

	as.evaluateAdaptive(1.0, rates, d.ActiveStreams())

	if as.probeState != probeIdle {
		t.Fatalf("expected probeState=probeIdle, got %d", as.probeState)
	}
	if as.probeCooldown != probeCooldownWindows {
		t.Fatalf("expected probeCooldown=%d, got %d", probeCooldownWindows, as.probeCooldown)
	}
	if d.ActiveStreams() != 1 {
		t.Fatalf("expected 1 active stream after failed probe revert, got %d", d.ActiveStreams())
	}

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateStable {
		t.Fatalf("expected snapshot state Stable, got %d", snap.State)
	}
	if snap.ProbeActive {
		t.Fatal("expected probe_active=false after failed probe")
	}
}

func TestAutoScaler_Adaptive_ScaleDownSetsCooldown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  4,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-adaptive-scale-down",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})

	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Hysteresis: 2,
		Logger:     logger,
		Mode:       "adaptive",
	})

	rates := RateSample{
		ProducerBps: 20,
		DrainBps:    100,
	}

	// Primeira janela abaixo do threshold: ainda sem scale-down.
	as.evaluateAdaptive(0.4, rates, d.ActiveStreams())
	if d.ActiveStreams() != 2 {
		t.Fatalf("expected 2 active streams after first low-efficiency window, got %d", d.ActiveStreams())
	}

	// Segunda janela consecutiva: dispara scale-down e cooldown.
	as.evaluateAdaptive(0.4, rates, d.ActiveStreams())
	if d.ActiveStreams() != 1 {
		t.Fatalf("expected 1 active stream after adaptive scale-down, got %d", d.ActiveStreams())
	}
	if as.probeCooldown != scaleDownCooldown {
		t.Fatalf("expected probeCooldown=%d after scale-down, got %d", scaleDownCooldown, as.probeCooldown)
	}

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateScaleDown {
		t.Fatalf("expected snapshot state ScaleDown, got %d", snap.State)
	}
}

func TestSender_ErrOffsetExpired_MarksStreamDead(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := NewDispatcher(DispatcherConfig{
		MaxStreams:  1,
		BufferSize:  1024,
		ChunkSize:   256,
		SessionID:   "test-expired",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})

	activateStreamManually(d, 0, &mockConn{})
	stream := d.streams[0]

	// Escreve dados no ring buffer
	data := make([]byte, 256)
	stream.rb.Write(data)

	// Avança o tail além de onde o sender está (offset=0)
	// Isso simula o cenário onde SACKs avançaram o RB e dados foram liberados
	stream.rb.Advance(256)

	// Sender deve obter ErrOffsetExpired ao ler do offset 0
	// Marca o stream como dead e envia erro no senderErr
	d.startSenderWithRetry(0)

	select {
	case err := <-stream.senderErr:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !stream.dead.Load() {
			t.Fatal("stream should be marked as dead after ErrOffsetExpired")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sender did not return within timeout")
	}
}

func TestSender_ResumeValidation_ContainsRange(t *testing.T) {
	// Testa que ContainsRange detecta quando o RB não contém mais
	// os dados no resumeOffset — cenário de flow rotation com dados perdidos
	rb := NewRingBuffer(128)

	// Escreve 64 bytes e avança tail
	data := make([]byte, 64)
	rb.Write(data)
	rb.Advance(64)

	// Escreve mais 64 bytes (head=128, tail=64)
	rb.Write(data)

	// Offset 0 não está mais no buffer — ContainsRange deve retornar false
	if rb.ContainsRange(0, 8) {
		t.Fatal("expected ContainsRange(0,8) = false after Advance(64)")
	}

	// Offset 64 está disponível
	if !rb.ContainsRange(64, 8) {
		t.Fatal("expected ContainsRange(64,8) = true")
	}

	// Offset 120 + 16 bytes = 136 excede head (128) — false
	if rb.ContainsRange(120, 16) {
		t.Fatal("expected ContainsRange(120,16) = false (exceeds head)")
	}
}
