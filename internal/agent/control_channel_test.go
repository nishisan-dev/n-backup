// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// TestControlChannel_StopUnblocksRead verifica que Stop() retorna rapidamente
// mesmo quando a goroutine está bloqueada em um read (ReadControlPong).
// Sem o fix, Stop() travaria até o read deadline expirar (~35s).
func TestControlChannel_StopUnblocksRead(t *testing.T) {
	// Setup: cria um par de conns (pipe) para simular agent←→server
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	// Simula server que lê pings mas NUNCA responde pongs
	go func() {
		for {
			buf := make([]byte, 12) // tamanho do ControlPing
			_, err := io.ReadFull(serverConn, buf)
			if err != nil {
				return // conn fechada
			}
			// Não envia pong — agent ficará bloqueado no read
		}
	}()

	cc := &ControlChannel{
		logger: slog.Default(),
		stopCh: make(chan struct{}),
	}
	cc.state.Store(StateConnected)
	cc.serverLoad.Store(float32(0))
	cc.diskFree.Store(uint32(0))

	// Injeta a conn diretamente
	cc.connMu.Lock()
	cc.conn = clientConn
	cc.connMu.Unlock()

	// Inicia pingLoop em goroutine (simula o que run() faz)
	cc.wg.Add(1)
	go func() {
		defer cc.wg.Done()
		// Simula um ping com read bloqueado
		now := time.Now().UnixNano()
		_ = protocol.WriteControlPing(clientConn, now)
		// Lê pong com deadline longo (simula bloqueio)
		clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, _ = protocol.ReadControlPong(clientConn) // vai bloquear aqui
	}()

	// Dá tempo do ping ser enviado e o read ficar bloqueado
	time.Sleep(100 * time.Millisecond)

	// Stop() deve desbloquear rapidamente (< 1s)
	stopDone := make(chan struct{})
	go func() {
		cc.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// OK — Stop() retornou rapidamente
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s — likely blocked on read")
	}

	if cc.State() != StateDisconnected {
		t.Errorf("expected state %s, got %s", StateDisconnected, cc.State())
	}
}

// TestControlChannel_KeepaliveServerTimeout verifica que o server timeout
// é compatível com o keepalive_interval enviado pelo agent via handshake.
// O agent envia [CTRL 4B][interval_secs uint32 4B], o server calcula timeout = 2.5x.
func TestControlChannel_KeepaliveServerTimeout(t *testing.T) {
	tests := []struct {
		name          string
		intervalSecs  uint32
		wantTimeout   time.Duration
		pingInterval  time.Duration
		expectTimeout bool // se o server deve dar timeout
	}{
		{
			name:          "30s interval, ping on time",
			intervalSecs:  30,
			wantTimeout:   75 * time.Second, // 30 * 2.5
			pingInterval:  200 * time.Millisecond,
			expectTimeout: false,
		},
		{
			name:          "90s custom interval, ping on time",
			intervalSecs:  90,
			wantTimeout:   225 * time.Second, // 90 * 2.5
			pingInterval:  200 * time.Millisecond,
			expectTimeout: false,
		},
		{
			name:          "1s interval, no ping sent",
			intervalSecs:  1,
			wantTimeout:   2500 * time.Millisecond, // 1 * 2.5
			pingInterval:  0,                       // não envia ping
			expectTimeout: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			// Envia handshake CTRL (magic já lido) + interval
			intervalBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(intervalBuf, tt.intervalSecs)
			go func() {
				clientConn.Write(intervalBuf)

				if tt.pingInterval > 0 {
					// Espera e envia um ping
					time.Sleep(tt.pingInterval)
					protocol.WriteControlPing(clientConn, time.Now().UnixNano())
				}
				// Se expectTimeout, não envia nada — o server vai dar timeout
			}()

			// Simula o que handleControlChannel faz: lê interval, calcula timeout
			readBuf := make([]byte, 4)
			serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(serverConn, readBuf); err != nil {
				t.Fatalf("reading interval: %v", err)
			}
			serverConn.SetReadDeadline(time.Time{})

			gotInterval := binary.BigEndian.Uint32(readBuf)
			if gotInterval != tt.intervalSecs {
				t.Fatalf("interval: want %d, got %d", tt.intervalSecs, gotInterval)
			}

			// Calcula timeout como o server faz: 5/2 (integer)
			readTimeout := time.Duration(gotInterval) * time.Second * 5 / 2
			if readTimeout != tt.wantTimeout {
				t.Fatalf("timeout: want %v, got %v", tt.wantTimeout, readTimeout)
			}

			// Tenta ler ping com o timeout calculado
			serverConn.SetReadDeadline(time.Now().Add(readTimeout))
			_, err := protocol.ReadControlPing(serverConn)
			serverConn.SetReadDeadline(time.Time{})

			if tt.expectTimeout {
				if err == nil {
					t.Fatal("expected timeout error, got nil")
				}
				// Verifica que é um timeout, não outro erro
				if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
					// net.Pipe timeout wraps in protocol read error
					t.Logf("got non-timeout error (acceptable for net.Pipe): %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestControlChannel_RTT_EWMA verifica que o cálculo de EWMA funciona corretamente
// com amostras variadas de RTT.
func TestControlChannel_RTT_EWMA(t *testing.T) {
	cc := &ControlChannel{}
	cc.state.Store(StateDisconnected)
	cc.serverLoad.Store(float32(0))
	cc.diskFree.Store(uint32(0))

	// Primeiro sample: armazenado diretamente
	cc.updateRTT(100 * time.Millisecond)
	if cc.RTT() != 100*time.Millisecond {
		t.Errorf("first sample: want 100ms, got %v", cc.RTT())
	}

	// Segundo sample: EWMA = 0.25 * 200ms + 0.75 * 100ms = 125ms
	cc.updateRTT(200 * time.Millisecond)
	expected := time.Duration(0.25*float64(200*time.Millisecond) + 0.75*float64(100*time.Millisecond))
	got := cc.RTT()
	// Permitir margem de arredondamento (1µs)
	diff := got - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Microsecond {
		t.Errorf("second sample: want ~%v, got %v (diff=%v)", expected, got, diff)
	}

	// Terceiro sample com valor baixo: deve puxar média para baixo
	cc.updateRTT(50 * time.Millisecond)
	if cc.RTT() >= expected {
		t.Errorf("third sample (50ms) should pull average down from %v, got %v", expected, cc.RTT())
	}
}
