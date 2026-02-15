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
