// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// AutoScaler monitora a eficiência do dispatcher e ajusta o número de streams.
// A cada intervalo (15s), calcula efficiency = producerRate / (drainRate × activeStreams).
// Usa histerese de 3 janelas consecutivas para evitar oscilações.
type AutoScaler struct {
	dispatcher *Dispatcher
	interval   time.Duration
	logger     *slog.Logger

	// Histerese: conta janelas consecutivas acima/abaixo do threshold
	scaleUpCount   int
	scaleDownCount int
	hysteresis     int // janelas necessárias para ação (default 3)

	// Última amostra para log nos métodos scale
	lastEfficiency float64
	lastRates      RateSample

	// Estado
	running int32 // atomic
}

// AutoScalerConfig contém parâmetros do auto-scaler.
type AutoScalerConfig struct {
	Dispatcher *Dispatcher
	Interval   time.Duration // default 15s
	Hysteresis int           // janelas para ação (default 3)
	Logger     *slog.Logger
}

// NewAutoScaler cria um novo auto-scaler.
func NewAutoScaler(cfg AutoScalerConfig) *AutoScaler {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Hysteresis <= 0 {
		cfg.Hysteresis = 3
	}

	return &AutoScaler{
		dispatcher: cfg.Dispatcher,
		interval:   cfg.Interval,
		hysteresis: cfg.Hysteresis,
		logger:     cfg.Logger,
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

// evaluate avalia a eficiência e decide scale-up/down.
//
// Condições conforme o plano:
//   - efficiency > 1.0 por 3 janelas → scale-up (+1 stream, até maxStreams)
//   - efficiency < 0.7 por 3 janelas → scale-down (-1 stream, mínimo 1)
//   - 0.7 ≤ efficiency ≤ 1.0 → estável (reset contadores)
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
		return
	}

	efficiency := rates.ProducerBps / rates.DrainBps

	// Armazena para uso nos logs de scale-up/down
	as.lastEfficiency = efficiency
	as.lastRates = rates

	// Diagnóstico: identifica gargalo producer vs consumer
	bottleneck := "balanced"
	if rates.ProducerBlockedMs > rates.SenderIdleMs && rates.ProducerBlockedMs > 100 {
		bottleneck = "network" // producer bloqueado = rede lenta
	} else if rates.SenderIdleMs > rates.ProducerBlockedMs && rates.SenderIdleMs > 100 {
		bottleneck = "producer" // senders ociosos = tar.gz lento
	}

	as.logger.Debug("auto-scaler evaluation",
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

	switch {
	case efficiency > 1.0:
		// Produtor mais rápido que os drains — precisa de mais streams
		as.scaleDownCount = 0
		as.scaleUpCount++

		if as.scaleUpCount >= as.hysteresis {
			as.scaleUp()
			as.scaleUpCount = 0
		}

	case efficiency < 0.7:
		// Drains subutilizados — pode remover um stream
		as.scaleUpCount = 0
		as.scaleDownCount++

		if as.scaleDownCount >= as.hysteresis {
			as.scaleDown()
			as.scaleDownCount = 0
		}

	default:
		// Estável — reset contadores
		as.scaleUpCount = 0
		as.scaleDownCount = 0
	}
}

// scaleUp ativa +1 stream.
func (as *AutoScaler) scaleUp() {
	active := as.dispatcher.ActiveStreams()
	if active >= as.dispatcher.maxStreams {
		as.logger.Debug("auto-scaler: already at max streams", "max", as.dispatcher.maxStreams)
		return
	}

	nextIdx := active // próximo stream a ativar
	if err := as.dispatcher.ActivateStream(nextIdx); err != nil {
		as.logger.Warn("auto-scaler: scale-up failed", "stream", nextIdx, "error", err)
		return
	}

	as.logger.Info("auto-scaler: scale-up",
		"reason", "producer faster than drains (efficiency > 1.0)",
		"efficiency", as.lastEfficiency,
		"producerMBs", as.lastRates.ProducerBps/(1024*1024),
		"drainMBs", as.lastRates.DrainBps/(1024*1024),
		"activeStreams", as.dispatcher.ActiveStreams(),
		"maxStreams", as.dispatcher.maxStreams,
	)
}

// scaleDown desativa 1 stream (o último ativo).
func (as *AutoScaler) scaleDown() {
	active := as.dispatcher.ActiveStreams()
	if active <= 1 {
		as.logger.Debug("auto-scaler: already at minimum streams")
		return
	}

	lastIdx := active - 1
	as.dispatcher.DeactivateStream(lastIdx)

	as.logger.Info("auto-scaler: scale-down",
		"reason", "drains underutilized (efficiency < 0.7)",
		"efficiency", as.lastEfficiency,
		"producerMBs", as.lastRates.ProducerBps/(1024*1024),
		"drainMBs", as.lastRates.DrainBps/(1024*1024),
		"activeStreams", as.dispatcher.ActiveStreams(),
		"maxStreams", as.dispatcher.maxStreams,
	)
}
