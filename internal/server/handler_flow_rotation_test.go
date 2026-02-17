// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

type testConn struct {
	closed atomic.Bool
}

func (c *testConn) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (c *testConn) Write(_ []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c *testConn) Close() error                       { c.closed.Store(true); return nil }
func (c *testConn) LocalAddr() net.Addr                { return testAddr("local") }
func (c *testConn) RemoteAddr() net.Addr               { return testAddr("remote") }
func (c *testConn) SetDeadline(_ time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(_ time.Time) error { return nil }

func newFlowRotationTestHandler() (*Handler, *sync.Map) {
	sessions := &sync.Map{}
	cfg := &config.ServerConfig{
		FlowRotation: config.FlowRotationConfig{
			Enabled:    true,
			MinMBps:    1.0,
			EvalWindow: 1 * time.Second,
			Cooldown:   1 * time.Second,
		},
	}

	h := NewHandler(cfg, slog.Default(), &sync.Map{}, sessions)
	return h, sessions
}

func newParallelSessionForFlowTest() *ParallelSession {
	ps := &ParallelSession{
		AgentName: "agent-test",
		Done:      make(chan struct{}),
	}
	return ps
}

func TestEvaluateFlowRotation_IdleStreamDoesNotRotate(t *testing.T) {
	h, sessions := newFlowRotationTestHandler()
	ps := newParallelSessionForFlowTest()
	conn := &testConn{}

	trafficCounter := &atomic.Int64{}
	ps.StreamTrafficIn.Store(uint8(0), trafficCounter)
	ps.StreamConns.Store(uint8(0), conn)
	ps.StreamSlowSince.Store(uint8(0), time.Now().Add(-2*time.Second))

	sessions.Store("session-idle", ps)

	h.evaluateFlowRotation(15)

	if conn.closed.Load() {
		t.Fatal("idle stream should not be closed by flow rotation")
	}
	if _, ok := ps.StreamSlowSince.Load(uint8(0)); ok {
		t.Fatal("idle stream should clear slow marker")
	}
	tickBytes, ok := ps.StreamTickBytes.Load(uint8(0))
	if !ok {
		t.Fatal("expected tick bytes snapshot for idle stream")
	}
	if tickBytes.(int64) != 0 {
		t.Fatalf("expected idle tick bytes = 0, got %d", tickBytes.(int64))
	}
}

func TestEvaluateFlowRotation_LowThroughputActiveStreamRotates(t *testing.T) {
	h, sessions := newFlowRotationTestHandler()
	ps := newParallelSessionForFlowTest()
	conn := &testConn{}

	trafficCounter := &atomic.Int64{}
	trafficCounter.Add(1024) // >0 bytes no intervalo, mas muito abaixo de 1 MB/s
	ps.StreamTrafficIn.Store(uint8(0), trafficCounter)
	ps.StreamConns.Store(uint8(0), conn)
	ps.StreamSlowSince.Store(uint8(0), time.Now().Add(-2*time.Second))

	sessions.Store("session-low-throughput", ps)

	h.evaluateFlowRotation(15)

	if !conn.closed.Load() {
		t.Fatal("low throughput active stream should be rotated")
	}
	if _, ok := ps.StreamSlowSince.Load(uint8(0)); ok {
		t.Fatal("slow marker should be cleared after rotation")
	}
	if _, ok := ps.StreamLastReset.Load(uint8(0)); !ok {
		t.Fatal("last reset timestamp should be stored after rotation")
	}
	tickBytes, ok := ps.StreamTickBytes.Load(uint8(0))
	if !ok {
		t.Fatal("expected tick bytes snapshot for active stream")
	}
	if tickBytes.(int64) != 1024 {
		t.Fatalf("expected tick bytes = 1024, got %d", tickBytes.(int64))
	}
}

// --- Testes de Graceful Flow Rotation (Fase 3) ---

// controlTestConn é uma testConn que também captura writes (para verificar ControlRotate enviado).
type controlTestConn struct {
	testConn
	writeBuf []byte
	mu       sync.Mutex
}

func (c *controlTestConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeBuf = append(c.writeBuf, p...)
	return len(p), nil
}

func TestEvaluateFlowRotation_GracefulWithControlChannel(t *testing.T) {
	h, sessions := newFlowRotationTestHandler()
	ps := newParallelSessionForFlowTest()
	dataConn := &testConn{}

	trafficCounter := &atomic.Int64{}
	trafficCounter.Add(1024) // low throughput
	ps.StreamTrafficIn.Store(uint8(0), trafficCounter)
	ps.StreamConns.Store(uint8(0), dataConn)
	ps.StreamSlowSince.Store(uint8(0), time.Now().Add(-2*time.Second))

	sessions.Store("session-graceful", ps)

	// Registra control conn para o agent
	ctrlConn := &controlTestConn{}
	mu := &sync.Mutex{}
	h.controlConns.Store("agent-test", &ControlConnInfo{Conn: ctrlConn, RemoteAddr: "test:1234", KeepaliveS: 30})
	h.controlConnsMu.Store("agent-test", mu)

	// Simula ACK chegando quase imediatamente em goroutine separada
	go func() {
		// Espera um pouco para o rotateStream ter tempo de criar o canal RotatePending
		time.Sleep(50 * time.Millisecond)
		if ackCh, ok := ps.RotatePending.Load(uint8(0)); ok {
			ackCh.(chan struct{}) <- struct{}{}
		}
	}()

	h.evaluateFlowRotation(15)

	// Verifica que o data conn foi fechado (graceful, após ACK)
	if !dataConn.closed.Load() {
		t.Fatal("data conn should be closed after graceful rotation")
	}

	// Verifica que ControlRotate foi escrito no control conn
	ctrlConn.mu.Lock()
	if len(ctrlConn.writeBuf) == 0 {
		t.Fatal("expected ControlRotate to be written to control conn")
	}
	// Verifica magic CROT (4 bytes)
	if string(ctrlConn.writeBuf[:4]) != "CROT" {
		t.Fatalf("expected CROT magic, got %q", ctrlConn.writeBuf[:4])
	}
	ctrlConn.mu.Unlock()
}

func TestEvaluateFlowRotation_FallbackWithoutControlChannel(t *testing.T) {
	h, sessions := newFlowRotationTestHandler()
	ps := newParallelSessionForFlowTest()
	dataConn := &testConn{}

	trafficCounter := &atomic.Int64{}
	trafficCounter.Add(1024) // low throughput
	ps.StreamTrafficIn.Store(uint8(0), trafficCounter)
	ps.StreamConns.Store(uint8(0), dataConn)
	ps.StreamSlowSince.Store(uint8(0), time.Now().Add(-2*time.Second))

	sessions.Store("session-no-ctrl", ps)

	// Sem registrar control conn — deve fazer fallback para close abrupto
	h.evaluateFlowRotation(15)

	if !dataConn.closed.Load() {
		t.Fatal("data conn should be closed by abrupt fallback")
	}
}

func TestEvaluateFlowRotation_FallbackOnACKTimeout(t *testing.T) {
	// Reduz rotateACKTimeout para o teste ser rápido
	// Na implementação real é 10s, mas vamos testar com registro de control
	// conn mas sem enviar ACK — deve fazer timeout e close abrupto.
	h, sessions := newFlowRotationTestHandler()
	ps := newParallelSessionForFlowTest()
	dataConn := &testConn{}

	trafficCounter := &atomic.Int64{}
	trafficCounter.Add(1024) // low throughput
	ps.StreamTrafficIn.Store(uint8(0), trafficCounter)
	ps.StreamConns.Store(uint8(0), dataConn)
	ps.StreamSlowSince.Store(uint8(0), time.Now().Add(-2*time.Second))

	sessions.Store("session-timeout", ps)

	// Registra control conn mas NÃO envia ACK
	ctrlConn := &controlTestConn{}
	mu := &sync.Mutex{}
	h.controlConns.Store("agent-test", &ControlConnInfo{Conn: ctrlConn, RemoteAddr: "test:1234", KeepaliveS: 30})
	h.controlConnsMu.Store("agent-test", mu)

	// evaluateFlowRotation vai enviar ControlRotate, esperar ACK por rotateACKTimeout (10s),
	// e então fazer fallback. Para não esperar 10s no teste, vamos rodar em goroutine com timeout.
	done := make(chan struct{})
	go func() {
		h.evaluateFlowRotation(15)
		close(done)
	}()

	select {
	case <-done:
		// OK — evaluateFlowRotation retornou
	case <-time.After(15 * time.Second):
		t.Fatal("evaluateFlowRotation should have timed out and returned")
	}

	if !dataConn.closed.Load() {
		t.Fatal("data conn should be closed by timeout fallback")
	}

	// Verifica que ControlRotate foi enviado antes do timeout
	ctrlConn.mu.Lock()
	if len(ctrlConn.writeBuf) == 0 {
		t.Fatal("expected ControlRotate to be sent before timeout")
	}
	ctrlConn.mu.Unlock()
}

// --- Testes de streamStatus ---

func TestStreamStatus_Disconnected(t *testing.T) {
	// Stream inativo (sem conexão no StreamConns) → "disconnected"
	got := streamStatus(false, 0, "", time.Minute)
	if got != "disconnected" {
		t.Fatalf("expected 'disconnected', got %q", got)
	}
}

func TestStreamStatus_Running(t *testing.T) {
	got := streamStatus(true, 5, "", time.Minute)
	if got != "running" {
		t.Fatalf("expected 'running', got %q", got)
	}
}

func TestStreamStatus_Idle(t *testing.T) {
	got := streamStatus(true, 30, "", time.Minute)
	if got != "idle" {
		t.Fatalf("expected 'idle', got %q", got)
	}
}

func TestStreamStatus_Degraded(t *testing.T) {
	got := streamStatus(true, 120, "", time.Minute)
	if got != "degraded" {
		t.Fatalf("expected 'degraded', got %q", got)
	}
}

func TestStreamStatus_Slow(t *testing.T) {
	// Slow since recente (dentro da eval window)
	slowSince := time.Now().Add(-10 * time.Second).Format(time.RFC3339)
	got := streamStatus(true, 5, slowSince, time.Minute)
	if got != "slow" {
		t.Fatalf("expected 'slow', got %q", got)
	}
}

func TestStreamStatus_DegradedFromSlow(t *testing.T) {
	// Slow since ultrapassou eval window → degraded
	slowSince := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	got := streamStatus(true, 5, slowSince, time.Minute)
	if got != "degraded" {
		t.Fatalf("expected 'degraded', got %q", got)
	}
}
