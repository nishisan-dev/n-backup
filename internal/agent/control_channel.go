// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"crypto/tls"
	"fmt"
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

// Version é a versão do agent, preenchida via ldflags no build (-X ...Version=x.y.z).
var Version = "dev"

// ControlChannel gerencia uma conexão TLS persistente com o server para
// keep-alive (PING/PONG), medição contínua de RTT, pre-flight check,
// e recepção de comandos do server (ControlRotate para flow rotation graceful).
type ControlChannel struct {
	cfg    *config.AgentConfig
	logger *slog.Logger

	// Conexão gerenciada
	conn   net.Conn
	connMu sync.Mutex

	// Mutex de write: protege writes concorrentes na conn
	// (pingWriter e SendRotateACK podem escrever simultaneamente)
	writeMu sync.Mutex

	// State machine (atômico para reads lock-free)
	state atomic.Value // string

	// RTT EWMA em nanoseconds (atômico)
	rttNanos atomic.Int64

	// Server metrics
	serverLoad atomic.Value // float32
	diskFree   atomic.Value // uint32

	// Callback chamado quando o server envia ControlRotate.
	// A função deve drenar o stream e retornar.
	onRotate func(streamIndex uint8)

	// Callback que retorna dados de progresso do backup em andamento.
	// Chamado a cada ping tick para enviar ControlProgress ao server.
	progressProvider func() (totalObjects, objectsSent uint32, walkComplete bool)

	// Callback que retorna stats do sistema.
	statsProvider func() *protocol.ControlStats

	// Callback que retorna stats do auto-scaler.
	autoScaleStatsProvider func() *protocol.ControlAutoScaleStats

	// Lifecycle
	stopCh chan struct{}
	stopMu sync.Once
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

// SetOnRotate define o callback chamado quando o server envia ControlRotate.
// Deve ser chamado antes de Start().
func (cc *ControlChannel) SetOnRotate(fn func(streamIndex uint8)) {
	cc.onRotate = fn
}

// SetProgressProvider define o callback que fornece dados de progresso do backup.
// Chamado a cada ping tick; quando retorna totalObjects > 0, envia ControlProgress ao server.
func (cc *ControlChannel) SetProgressProvider(fn func() (totalObjects, objectsSent uint32, walkComplete bool)) {
	cc.progressProvider = fn
}

// SetStatsProvider define o callback que fornece estatísticas do sistema.
// Chamado a cada ping tick; envia ControlStats ao server.
func (cc *ControlChannel) SetStatsProvider(fn func() *protocol.ControlStats) {
	cc.statsProvider = fn
}

// SetAutoScaleStatsProvider define o callback que fornece estatísticas do auto-scaler.
// Chamado a cada ping tick; envia ControlAutoScaleStats ao server.
func (cc *ControlChannel) SetAutoScaleStatsProvider(fn func() *protocol.ControlAutoScaleStats) {
	cc.autoScaleStatsProvider = fn
}

// SendProgress envia um frame ControlProgress ao server imediatamente.
// Thread-safe via writeMu.
func (cc *ControlChannel) SendProgress(totalObjects, objectsSent uint32, walkComplete bool) error {
	cc.connMu.Lock()
	conn := cc.conn
	cc.connMu.Unlock()

	if conn == nil {
		return nil
	}

	cc.writeMu.Lock()
	err := protocol.WriteControlProgress(conn, totalObjects, objectsSent, walkComplete)
	cc.writeMu.Unlock()

	if err != nil {
		cc.logger.Warn("failed to send ControlProgress", "error", err)
	}
	return err
}

// SendStats envia um frame ControlStats ao server imediatamente.
// Thread-safe via writeMu.
func (cc *ControlChannel) SendStats(stats *protocol.ControlStats) error {
	cc.connMu.Lock()
	conn := cc.conn
	cc.connMu.Unlock()

	if conn == nil {
		return nil
	}

	cc.writeMu.Lock()
	err := protocol.WriteControlStats(conn, stats.CPUPercent, stats.MemoryPercent, stats.DiskUsagePercent, stats.LoadAverage)
	cc.writeMu.Unlock()

	if err != nil {
		cc.logger.Warn("failed to send ControlStats", "error", err)
	}
	return err
}

// SendRotateACK envia ControlRotateACK ao server pelo canal de controle.
// Thread-safe via writeMu.
func (cc *ControlChannel) SendRotateACK(streamIndex uint8) error {
	cc.connMu.Lock()
	conn := cc.conn
	cc.connMu.Unlock()

	if conn == nil {
		return nil // sem conexão, ignora
	}

	cc.writeMu.Lock()
	err := protocol.WriteControlRotateACK(conn, streamIndex)
	cc.writeMu.Unlock()

	if err != nil {
		cc.logger.Warn("failed to send ControlRotateACK", "error", err, "stream", streamIndex)
	}
	return err
}

// SendIngestionDone envia ControlIngestionDone ao server pelo canal de controle.
// Sinaliza que o agent terminou de enviar todos os chunks com sucesso.
// Retorna erro se o control channel estiver desconectado.
// Thread-safe via writeMu.
func (cc *ControlChannel) SendIngestionDone(sessionID string) error {
	cc.connMu.Lock()
	conn := cc.conn
	cc.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("control channel unavailable: cannot send ControlIngestionDone for session %s", sessionID)
	}

	cc.writeMu.Lock()
	err := protocol.WriteControlIngestionDone(conn, sessionID)
	cc.writeMu.Unlock()

	if err != nil {
		cc.logger.Warn("failed to send ControlIngestionDone", "error", err, "session", sessionID)
	}
	return err
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
	cc.stopMu.Do(func() {
		close(cc.stopCh)
	})

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
	intervalSecs := uint32(math.Ceil(cc.cfg.Daemon.ControlChannel.KeepaliveInterval.Seconds()))
	if intervalSecs == 0 {
		intervalSecs = 1
	}
	handshake[4] = byte(intervalSecs >> 24)
	handshake[5] = byte(intervalSecs >> 16)
	handshake[6] = byte(intervalSecs >> 8)
	handshake[7] = byte(intervalSecs)
	if _, err := tlsConn.Write(handshake); err != nil {
		tlsConn.Close()
		return err
	}

	// Envia version do agent (string terminada em newline)
	if _, err := tlsConn.Write([]byte(Version + "\n")); err != nil {
		tlsConn.Close()
		return err
	}

	// Envia stats iniciais (16B: CPU, Mem, Disk, Load)
	var cpu, mem, disk, load float32
	if cc.statsProvider != nil {
		if st := cc.statsProvider(); st != nil {
			cpu = st.CPUPercent
			mem = st.MemoryPercent
			disk = st.DiskUsagePercent
			load = st.LoadAverage
		}
	}
	if err := protocol.WriteControlStatsPayload(tlsConn, cpu, mem, disk, load); err != nil {
		tlsConn.Close()
		return err
	}

	cc.connMu.Lock()
	cc.conn = tlsConn
	cc.connMu.Unlock()

	return nil
}

// pingLoop roda em full-duplex: um ping writer envia pings periódicos,
// e um frame reader lê respostas e comandos assíncronos do server.
// Retorna quando detecta desconexão, erro ou stop signal.
func (cc *ControlChannel) pingLoop() {
	cc.connMu.Lock()
	conn := cc.conn
	cc.connMu.Unlock()

	if conn == nil {
		return
	}

	ccCfg := cc.cfg.Daemon.ControlChannel

	// Canal para sinalizar que o reader ou writer terminou
	done := make(chan struct{})

	// --- Frame Reader goroutine ---
	// Lê qualquer frame do server (PONG, ControlRotate) e despacha.
	go func() {
		defer func() {
			select {
			case done <- struct{}{}:
			default:
			}
		}()

		readTimeout := ccCfg.KeepaliveInterval + 5*time.Second
		missedPings := 0

		for {
			select {
			case <-cc.stopCh:
				return
			default:
			}

			conn.SetReadDeadline(time.Now().Add(readTimeout))

			// Lê magic de 4 bytes para determinar o tipo de frame
			magic, err := protocol.ReadControlMagic(conn)
			if err != nil {
				missedPings++
				if missedPings >= maxMissedPings {
					cc.state.Store(StateDegraded)
					cc.logger.Error("control channel degraded: max missed reads",
						"missed", missedPings, "error", err)
				} else {
					cc.logger.Warn("control channel read failed",
						"error", err, "missed", missedPings)
				}
				return
			}

			switch magic {
			case protocol.MagicControlPing:
				// Server respondeu PONG (mesmo magic CPNG)
				pong, err := protocol.ReadControlPongPayload(conn)
				if err != nil {
					cc.logger.Warn("control channel: reading pong payload", "error", err)
					return
				}

				// Calcula RTT
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

			case protocol.MagicControlRotate:
				// Server solicitou rotação graceful de um stream
				streamIdx, err := protocol.ReadControlRotatePayload(conn)
				if err != nil {
					cc.logger.Warn("control channel: reading rotate payload", "error", err)
					return
				}

				cc.logger.Info("control channel: received ControlRotate",
					"stream", streamIdx)

				// Executa em goroutine para não bloquear o reader.
				// O ACK DEVE ser enviado sempre — onRotate é opcional.
				go func(idx uint8) {
					defer func() {
						if r := recover(); r != nil {
							cc.logger.Error("control channel: onRotate panic recovered",
								"stream", idx, "panic", r)
						}
						// Envia ACK sempre, mesmo se onRotate panicar.
						cc.SendRotateACK(idx)
						cc.logger.Info("control channel: sent ControlRotateACK",
							"stream", idx)
					}()

					if cc.onRotate != nil {
						cc.onRotate(idx)
					} else {
						cc.logger.Warn("control channel: onRotate handler missing, sending ACK without drain",
							"stream", idx)
					}
				}(streamIdx)

			default:
				cc.logger.Warn("control channel: unknown magic from server",
					"magic", string(magic[:]))
				return
			}
		}
	}()

	// --- Ping Writer ---
	// Envia ControlPing periódico no mesmo loop de controle.
	ticker := time.NewTicker(ccCfg.KeepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cc.stopCh:
			return
		case <-done:
			// Reader terminou (erro ou timeout) — sai do loop
			return
		case <-ticker.C:
			// Envia ping com mutex de write
			now := time.Now().UnixNano()
			cc.writeMu.Lock()
			err := protocol.WriteControlPing(conn, now)
			// Coalescendo envio de progress com o mesmo tick de ping
			if err == nil && cc.progressProvider != nil {
				total, sent, walk := cc.progressProvider()
				if total > 0 {
					err = protocol.WriteControlProgress(conn, total, sent, walk)
				}
			}
			if err == nil && cc.statsProvider != nil {
				stats := cc.statsProvider()
				if stats != nil {
					err = protocol.WriteControlStats(conn, stats.CPUPercent, stats.MemoryPercent, stats.DiskUsagePercent, stats.LoadAverage)
				}
			}
			if err == nil && cc.autoScaleStatsProvider != nil {
				asStats := cc.autoScaleStatsProvider()
				if asStats != nil {
					err = protocol.WriteControlAutoScaleStats(conn, asStats)
				}
			}
			cc.writeMu.Unlock()

			if err != nil {
				cc.logger.Warn("control channel write failed", "error", err)
				return
			}
		}
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
