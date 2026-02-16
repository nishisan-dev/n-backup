// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"crypto/tls"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// ControlChannel state constants.
const (
	StateDisconnected = "disconnected"
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateDegraded     = "degraded"
)

// maxMissedPings é o número de pings sem resposta antes de considerar o server unreachable.
const maxMissedPings = 3

// ewmaAlpha é o fator de suavização para o EWMA do RTT.
const ewmaAlpha = 0.25

// ControlChannel gerencia uma conexão TLS persistente com o server para
// keep-alive (PING/PONG), medição contínua de RTT e pre-flight check.
type ControlChannel struct {
	cfg    *config.AgentConfig
	logger *slog.Logger

	// Conexão gerenciada
	conn   net.Conn
	connMu sync.Mutex

	// State machine (atômico para reads lock-free)
	state atomic.Value // string

	// RTT EWMA em nanoseconds (atômico)
	rttNanos atomic.Int64

	// Server metrics
	serverLoad atomic.Value // float32
	diskFree   atomic.Value // uint32

	// Lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewControlChannel cria um novo ControlChannel.
func NewControlChannel(cfg *config.AgentConfig, logger *slog.Logger) *ControlChannel {
	cc := &ControlChannel{
		cfg:    cfg,
		logger: logger.With("component", "control_channel"),
		stopCh: make(chan struct{}),
	}
	cc.state.Store(StateDisconnected)
	cc.serverLoad.Store(float32(0))
	cc.diskFree.Store(uint32(0))
	return cc
}

// Start inicia a goroutine de manutenção do canal de controle.
func (cc *ControlChannel) Start() {
	cc.wg.Add(1)
	go cc.run()
	cc.logger.Info("control channel started")
}

// Stop para o canal de controle e aguarda a goroutine terminar.
// Fecha a conexão primeiro para desbloquear qualquer read pendente.
func (cc *ControlChannel) Stop() {
	close(cc.stopCh)

	// Fecha conn ANTES de Wait para desbloquear reads bloqueados no pingLoop
	cc.connMu.Lock()
	if cc.conn != nil {
		cc.conn.Close()
	}
	cc.connMu.Unlock()

	cc.wg.Wait()

	// Nil-ifica a referência após goroutine terminar
	cc.connMu.Lock()
	cc.conn = nil
	cc.connMu.Unlock()

	cc.state.Store(StateDisconnected)
	cc.logger.Info("control channel stopped")
}

// IsConnected retorna true se o canal está no estado CONNECTED.
func (cc *ControlChannel) IsConnected() bool {
	return cc.state.Load().(string) == StateConnected
}

// RTT retorna o RTT médio calculado via EWMA. Retorna 0 se nunca medido.
func (cc *ControlChannel) RTT() time.Duration {
	return time.Duration(cc.rttNanos.Load())
}

// ServerLoad retorna a carga reportada pelo server (0.0 a 1.0).
func (cc *ControlChannel) ServerLoad() float32 {
	return cc.serverLoad.Load().(float32)
}

// State retorna o estado atual do canal de controle.
func (cc *ControlChannel) State() string {
	return cc.state.Load().(string)
}

// run é a goroutine principal do control channel.
// Conecta ao server e mantém o ping loop até ser parado.
func (cc *ControlChannel) run() {
	defer cc.wg.Done()

	ccCfg := cc.cfg.Daemon.ControlChannel
	delay := ccCfg.ReconnectDelay

	for {
		select {
		case <-cc.stopCh:
			return
		default:
		}

		cc.state.Store(StateConnecting)
		err := cc.connect()
		if err != nil {
			cc.logger.Warn("control channel connect failed", "error", err, "retry_in", delay)
			cc.state.Store(StateDisconnected)

			select {
			case <-cc.stopCh:
				return
			case <-time.After(delay):
			}

			// Exponential backoff
			delay = time.Duration(float64(delay) * 2)
			if delay > ccCfg.MaxReconnectDelay {
				delay = ccCfg.MaxReconnectDelay
			}
			continue
		}

		// Reset backoff on successful connect
		delay = ccCfg.ReconnectDelay
		cc.state.Store(StateConnected)
		cc.logger.Info("control channel connected", "server", cc.cfg.Server.Address)

		// Ping loop — roda até erro ou stop
		cc.pingLoop()

		// Cleanup conn
		cc.connMu.Lock()
		if cc.conn != nil {
			cc.conn.Close()
			cc.conn = nil
		}
		cc.connMu.Unlock()

		cc.state.Store(StateDisconnected)
		cc.logger.Info("control channel disconnected, will reconnect")
	}
}

// connect estabelece a conexão TLS, envia o magic "CTRL" e o keepalive_interval.
func (cc *ControlChannel) connect() error {
	tlsCfg, err := pki.NewClientTLSConfig(cc.cfg.TLS.CACert, cc.cfg.TLS.ClientCert, cc.cfg.TLS.ClientKey)
	if err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(cc.cfg.Server.Address)
	if err != nil {
		host = cc.cfg.Server.Address
	}
	tlsCfg.ServerName = host

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.Dial("tcp", cc.cfg.Server.Address)
	if err != nil {
		return err
	}

	tlsConn := tls.Client(rawConn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		rawConn.Close()
		return err
	}

	// Envia magic "CTRL" + keepalive_interval (uint32 big-endian, em segundos)
	// O server usa keepalive_interval para calcular o read timeout (2.5x)
	handshake := make([]byte, 8) // 4B magic + 4B interval
	copy(handshake[0:4], protocol.MagicControl[:])
	intervalSecs := uint32(cc.cfg.Daemon.ControlChannel.KeepaliveInterval.Seconds())
	handshake[4] = byte(intervalSecs >> 24)
	handshake[5] = byte(intervalSecs >> 16)
	handshake[6] = byte(intervalSecs >> 8)
	handshake[7] = byte(intervalSecs)
	if _, err := tlsConn.Write(handshake); err != nil {
		tlsConn.Close()
		return err
	}

	cc.connMu.Lock()
	cc.conn = tlsConn
	cc.connMu.Unlock()

	return nil
}

// pingLoop envia ControlPing periodicamente e lê ControlPong.
// Retorna quando detecta desconexão, erro ou stop signal.
func (cc *ControlChannel) pingLoop() {
	ccCfg := cc.cfg.Daemon.ControlChannel
	ticker := time.NewTicker(ccCfg.KeepaliveInterval)
	defer ticker.Stop()

	missedPings := 0
	pongDeadline := ccCfg.KeepaliveInterval + 5*time.Second // margem para o pong

	for {
		select {
		case <-cc.stopCh:
			return
		case <-ticker.C:
		}

		cc.connMu.Lock()
		conn := cc.conn
		cc.connMu.Unlock()

		if conn == nil {
			return
		}

		// Envia ping
		now := time.Now().UnixNano()
		if err := protocol.WriteControlPing(conn, now); err != nil {
			cc.logger.Warn("control channel ping write failed", "error", err)
			return
		}

		// Lê pong com deadline
		conn.SetReadDeadline(time.Now().Add(pongDeadline))
		pong, err := protocol.ReadControlPong(conn)
		conn.SetReadDeadline(time.Time{}) // remove deadline

		if err != nil {
			missedPings++
			cc.logger.Warn("control channel pong read failed",
				"error", err,
				"missed", missedPings,
			)

			if missedPings >= maxMissedPings {
				cc.state.Store(StateDegraded)
				cc.logger.Error("control channel degraded: max missed pings reached",
					"missed", missedPings,
				)
				return
			}
			continue
		}

		// Pong recebido — calcula RTT
		rttSample := time.Duration(time.Now().UnixNano() - pong.Timestamp)
		if rttSample < 0 {
			rttSample = 0
		}
		cc.updateRTT(rttSample)

		// Atualiza métricas do server
		cc.serverLoad.Store(pong.ServerLoad)
		cc.diskFree.Store(pong.DiskFree)

		missedPings = 0

		cc.logger.Debug("control channel pong received",
			"rtt", rttSample,
			"ewma_rtt", cc.RTT(),
			"server_load", pong.ServerLoad,
			"disk_free_mb", pong.DiskFree,
		)
	}
}

// updateRTT atualiza o RTT EWMA com um novo sample.
func (cc *ControlChannel) updateRTT(sample time.Duration) {
	current := cc.rttNanos.Load()
	if current == 0 {
		// Primeiro sample
		cc.rttNanos.Store(int64(sample))
		return
	}
	// EWMA: new = α * sample + (1-α) * current
	newRTT := ewmaAlpha*float64(sample) + (1-ewmaAlpha)*float64(current)
	cc.rttNanos.Store(int64(math.Round(newRTT)))
}
