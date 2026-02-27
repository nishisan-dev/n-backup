// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// AutoScaleSnapshot contém os dados da última avaliação do auto-scaler.
// Exportado para envio via protocolo ao server.
type AutoScaleSnapshot struct {
	Efficiency    float32
	ProducerMBs   float32
	DrainMBs      float32
	ActiveStreams uint8
	MaxStreams    uint8
	State         uint8 // protocol.AutoScaleState*
	ProbeActive   bool
}

// AutoScaler monitora a eficiência do dispatcher e ajusta o número de streams.
// Suporta dois modos:
//   - "efficiency": algoritmo original baseado em efficiency = producerRate / drainRate
//   - "adaptive": probe-and-measure que testa +1 stream e mede throughput
type AutoScaler struct {
	dispatcher *Dispatcher
	interval   time.Duration
	logger     *slog.Logger
	mode       string // "efficiency" | "adaptive"

	// Histerese: conta janelas consecutivas acima/abaixo do threshold
	scaleUpCount   int
	scaleDownCount int
	hysteresis     int // janelas necessárias para ação (default 3)

	// Última amostra para log nos métodos scale
	lastEfficiency float64
	lastRates      RateSample

	// Probe state (modo adaptive)
	probeState    int     // 0=idle, 1=probing, 2=cooldown
	probeBaseline float64 // throughput baseline antes do probe (bytes/s)
	probeStream   int     // índice ativado no probe atual (-1 quando nenhum)
	probeWindows  int     // janelas decorridas no probe
	probeCooldown int     // janelas restantes de cooldown

	// Snapshot exportado (thread-safe)
	snapshotMu   sync.RWMutex
	LastSnapshot AutoScaleSnapshot

	// Estado
	running int32 // atomic
}

// Probe states.
const (
	probeIdle     = 0
	probeProbing  = 1
	probeCooldown = 2
)

// Probe constants.
const (
	probeWindowsRequired = 3    // janelas para medir resultado do probe
	probeGainThreshold   = 0.05 // 5% de melhoria para manter
	probeCooldownWindows = 5    // janelas de cooldown após probe falho
	scaleDownCooldown    = 3    // janelas de cooldown após scale-down
	adaptiveScaleDownThr = 0.5  // threshold de scale-down no modo adaptive
)

// AutoScalerConfig contém parâmetros do auto-scaler.
type AutoScalerConfig struct {
	Dispatcher *Dispatcher
	Interval   time.Duration // default 15s
	Hysteresis int           // janelas para ação (default 3)
	Logger     *slog.Logger
	Mode       string // "efficiency" | "adaptive"
}

// NewAutoScaler cria um novo auto-scaler.
func NewAutoScaler(cfg AutoScalerConfig) *AutoScaler {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Hysteresis <= 0 {
		cfg.Hysteresis = 3
	}
	if cfg.Mode == "" {
		cfg.Mode = "efficiency"
	}

	return &AutoScaler{
		dispatcher:  cfg.Dispatcher,
		interval:    cfg.Interval,
		hysteresis:  cfg.Hysteresis,
		logger:      cfg.Logger,
		mode:        cfg.Mode,
		probeStream: -1,
	}
}

// Run inicia o loop do auto-scaler. Bloqueia até o contexto ser cancelado.
func (as *AutoScaler) Run(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&as.running, 0, 1) {
		return // já rodando
	}
	defer atomic.StoreInt32(&as.running, 0)

	ticker := time.NewTicker(as.interval)
	defer ticker.Stop()

	as.logger.Info("auto-scaler started",
		"mode", as.mode,
		"interval", as.interval,
		"hysteresis", as.hysteresis,
		"maxStreams", as.dispatcher.maxStreams,
	)

	for {
		select {
		case <-ctx.Done():
			as.logger.Info("auto-scaler stopped")
			return
		case <-ticker.C:
			as.evaluate()
		}
	}
}

// Snapshot retorna uma cópia thread-safe do último snapshot.
func (as *AutoScaler) Snapshot() AutoScaleSnapshot {
	as.snapshotMu.RLock()
	defer as.snapshotMu.RUnlock()
	return as.LastSnapshot
}

// evaluate avalia a eficiência e decide scale-up/down baseado no modo.
func (as *AutoScaler) evaluate() {
	rates := as.dispatcher.SampleRates()
	active := as.dispatcher.ActiveStreams()

	// Evita divisão por zero
	if rates.DrainBps <= 0 || active <= 0 {
		as.logger.Debug("auto-scaler: insufficient drain data",
			"producerBps", rates.ProducerBps,
			"drainBps", rates.DrainBps,
			"active", active,
		)
		as.updateSnapshot(0, rates, active, protocol.AutoScaleStateStable, false)
		return
	}

	efficiency := rates.ProducerBps / rates.DrainBps

	// Armazena para uso nos logs de scale-up/down
	as.lastEfficiency = efficiency
	as.lastRates = rates

	// Diagnóstico: identifica gargalo producer vs consumer
	bottleneck := "balanced"
	if rates.ProducerBlockedMs > rates.SenderIdleMs && rates.ProducerBlockedMs > 100 {
		bottleneck = "network"
	} else if rates.SenderIdleMs > rates.ProducerBlockedMs && rates.SenderIdleMs > 100 {
		bottleneck = "producer"
	}

	as.logger.Debug("auto-scaler evaluation",
		"mode", as.mode,
		"efficiency", efficiency,
		"producerBps", rates.ProducerBps,
		"drainBps", rates.DrainBps,
		"activeStreams", active,
		"scaleUpCount", as.scaleUpCount,
		"scaleDownCount", as.scaleDownCount,
		"producerBlockedMs", rates.ProducerBlockedMs,
		"senderIdleMs", rates.SenderIdleMs,
		"bottleneck", bottleneck,
	)

	switch as.mode {
	case "adaptive":
		as.evaluateAdaptive(efficiency, rates, active)
	default:
		as.evaluateEfficiency(efficiency, rates, active)
	}
}

// evaluateEfficiency implementa o algoritmo original baseado em thresholds de efficiency.
func (as *AutoScaler) evaluateEfficiency(efficiency float64, rates RateSample, active int) {
	switch {
	case efficiency > 1.0:
		// Produtor mais rápido que os drains — precisa de mais streams
		as.scaleDownCount = 0
		as.scaleUpCount++

		if as.scaleUpCount >= as.hysteresis {
			as.scaleUp("producer faster than drains (efficiency > 1.0)")
			as.scaleUpCount = 0
			as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateScalingUp, false)
			return
		}

	case efficiency < 0.7:
		// Drains subutilizados — pode remover um stream
		as.scaleUpCount = 0
		as.scaleDownCount++

		if as.scaleDownCount >= as.hysteresis {
			as.scaleDown("drains underutilized (efficiency < 0.7)")
			as.scaleDownCount = 0
			as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateScaleDown, false)
			return
		}

	default:
		// Estável — reset contadores
		as.scaleUpCount = 0
		as.scaleDownCount = 0
	}

	as.updateSnapshot(efficiency, rates, active, protocol.AutoScaleStateStable, false)
}

// evaluateAdaptive implementa o algoritmo probe-and-measure.
func (as *AutoScaler) evaluateAdaptive(efficiency float64, rates RateSample, active int) {
	totalThroughput := rates.ProducerBps + rates.DrainBps

	switch as.probeState {
	case probeIdle:
		// Cooldown ativo?
		if as.probeCooldown > 0 {
			as.probeCooldown--
			as.updateSnapshot(efficiency, rates, active, protocol.AutoScaleStateStable, false)
			return
		}

		// Scale-down: se efficiency muito baixa, reduz streams
		if efficiency < adaptiveScaleDownThr {
			as.scaleUpCount = 0
			as.scaleDownCount++

			if as.scaleDownCount >= as.hysteresis {
				as.scaleDown("adaptive: drains underutilized (efficiency < 0.5)")
				as.scaleDownCount = 0
				as.probeCooldown = scaleDownCooldown
				as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateScaleDown, false)
				return
			}
		} else {
			as.scaleDownCount = 0
		}

		// Tenta iniciar probe se tem headroom
		if active < as.dispatcher.maxStreams {
			as.scaleUpCount++
			if as.scaleUpCount >= as.hysteresis {
				// Inicia probe: ativa +1 stream e mede
				as.probeBaseline = totalThroughput
				as.probeWindows = 0
				as.probeState = probeProbing
				as.scaleUpCount = 0

				nextIdx := as.dispatcher.NextActivatableStream()
				if nextIdx < 0 {
					as.logger.Debug("auto-scaler: no stream available for probe")
					as.probeState = probeIdle
					as.probeBaseline = 0
					as.probeStream = -1
					as.updateSnapshot(efficiency, rates, active, protocol.AutoScaleStateStable, false)
					return
				}
				if err := as.dispatcher.ActivateStream(nextIdx); err != nil {
					as.logger.Warn("auto-scaler: probe activation failed",
						"stream", nextIdx, "error", err)
					as.probeState = probeIdle
					as.probeBaseline = 0
					as.probeStream = -1
					as.probeCooldown = probeCooldownWindows
					as.updateSnapshot(efficiency, rates, active, protocol.AutoScaleStateStable, false)
					return
				}
				as.probeStream = nextIdx

				as.logger.Info("auto-scaler: probe started",
					"baseline", as.probeBaseline/(1024*1024),
					"activeStreams", as.dispatcher.ActiveStreams(),
					"maxStreams", as.dispatcher.maxStreams,
				)
				as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateProbing, true)
				return
			}
		} else {
			as.scaleUpCount = 0
		}

		as.updateSnapshot(efficiency, rates, active, protocol.AutoScaleStateStable, false)

	case probeProbing:
		as.probeWindows++

		if as.probeWindows >= probeWindowsRequired {
			// Avalia resultado do probe
			gain := 0.0
			if as.probeBaseline > 0 {
				gain = (totalThroughput - as.probeBaseline) / as.probeBaseline
			}

			if gain >= probeGainThreshold {
				// Probe succeeded — mantém o stream
				as.logger.Info("auto-scaler: probe succeeded, keeping stream",
					"gain", gain,
					"baseline", as.probeBaseline/(1024*1024),
					"current", totalThroughput/(1024*1024),
					"activeStreams", as.dispatcher.ActiveStreams(),
				)
				as.probeState = probeIdle
				as.probeBaseline = 0
				as.probeStream = -1
				as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateScalingUp, false)
			} else {
				// Probe failed — reverte
				if as.probeStream >= 0 {
					as.dispatcher.DeactivateStream(as.probeStream)
				}
				as.logger.Info("auto-scaler: probe failed, reverting",
					"gain", gain,
					"baseline", as.probeBaseline/(1024*1024),
					"current", totalThroughput/(1024*1024),
					"activeStreams", as.dispatcher.ActiveStreams(),
				)
				as.probeState = probeIdle
				as.probeBaseline = 0
				as.probeStream = -1
				as.probeCooldown = probeCooldownWindows
				as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateStable, false)
			}
			return
		}

		// Ainda medindo
		as.updateSnapshot(efficiency, rates, as.dispatcher.ActiveStreams(), protocol.AutoScaleStateProbing, true)
	}
}

// scaleUp ativa +1 stream.
func (as *AutoScaler) scaleUp(reason string) {
	active := as.dispatcher.ActiveStreams()
	if active >= as.dispatcher.maxStreams {
		as.logger.Debug("auto-scaler: already at max streams", "max", as.dispatcher.maxStreams)
		return
	}

	nextIdx := as.dispatcher.NextActivatableStream()
	if nextIdx < 0 {
		as.logger.Debug("auto-scaler: no stream available for scale-up")
		return
	}
	if err := as.dispatcher.ActivateStream(nextIdx); err != nil {
		as.logger.Warn("auto-scaler: scale-up failed", "stream", nextIdx, "error", err)
		return
	}

	as.logger.Info("auto-scaler: scale-up",
		"reason", reason,
		"efficiency", as.lastEfficiency,
		"producerMBs", as.lastRates.ProducerBps/(1024*1024),
		"drainMBs", as.lastRates.DrainBps/(1024*1024),
		"activeStreams", as.dispatcher.ActiveStreams(),
		"maxStreams", as.dispatcher.maxStreams,
	)
}

// scaleDown desativa 1 stream (o último ativo).
func (as *AutoScaler) scaleDown(reason string) {
	active := as.dispatcher.ActiveStreams()
	if active <= 1 {
		as.logger.Debug("auto-scaler: already at minimum streams")
		return
	}

	lastIdx := as.dispatcher.LastActiveStream()
	if lastIdx <= 0 {
		as.logger.Debug("auto-scaler: no eligible stream for scale-down")
		return
	}
	as.dispatcher.DeactivateStream(lastIdx)

	as.logger.Info("auto-scaler: scale-down",
		"reason", reason,
		"efficiency", as.lastEfficiency,
		"producerMBs", as.lastRates.ProducerBps/(1024*1024),
		"drainMBs", as.lastRates.DrainBps/(1024*1024),
		"activeStreams", as.dispatcher.ActiveStreams(),
		"maxStreams", as.dispatcher.maxStreams,
	)
}

// updateSnapshot atualiza o snapshot thread-safe com os dados da última avaliação.
func (as *AutoScaler) updateSnapshot(efficiency float64, rates RateSample, active int, state uint8, probeActive bool) {
	as.snapshotMu.Lock()
	as.LastSnapshot = AutoScaleSnapshot{
		Efficiency:    float32(efficiency),
		ProducerMBs:   float32(rates.ProducerBps / (1024 * 1024)),
		DrainMBs:      float32(rates.DrainBps / (1024 * 1024)),
		ActiveStreams: uint8(active),
		MaxStreams:    uint8(as.dispatcher.maxStreams),
		State:         state,
		ProbeActive:   probeActive,
	}
	as.snapshotMu.Unlock()
}
