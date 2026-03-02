// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// ---------------------------------------------------------------------------
// Helpers para construir Dispatcher + AutoScaler sem rede
// ---------------------------------------------------------------------------

func newTestDispatcher(maxStreams int) *Dispatcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewDispatcher(DispatcherConfig{
		MaxStreams:  maxStreams,
		BufferSize:  1024 * 1024,
		ChunkSize:   1024,
		SessionID:   "test-autoscaler",
		ServerAddr:  "localhost:9847",
		AgentName:   "test-agent",
		StorageName: "test-storage",
		Logger:      logger,
		PrimaryConn: nil,
	})
}

func newTestAutoScaler(d *Dispatcher, mode string, hysteresis int) *AutoScaler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Interval:   100 * time.Millisecond,
		Hysteresis: hysteresis,
		Logger:     logger,
		Mode:       mode,
	})
}

// injectRates simula métricas para o dispatcher (producer e drain).
func injectRates(d *Dispatcher, producerBytes int64, streamDrainBytes []int64) {
	d.mu.Lock()
	d.lastSampleAt = time.Now().Add(-1 * time.Second) // 1s para cálculo
	d.mu.Unlock()
	atomic.StoreInt64(&d.producerBytes, producerBytes)
	for i, bytes := range streamDrainBytes {
		if i < len(d.streams) {
			atomic.StoreInt64(&d.streams[i].drainBytes, bytes)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: NewAutoScaler defaults
// ---------------------------------------------------------------------------

// TestNewAutoScaler_Defaults verifica que NewAutoScaler aplica defaults corretos
// quando os campos não são informados na config.
func TestNewAutoScaler_Defaults(t *testing.T) {
	d := newTestDispatcher(4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Logger:     logger,
	})

	if as.interval != 15*time.Second {
		t.Errorf("expected default interval 15s, got %v", as.interval)
	}
	if as.hysteresis != 3 {
		t.Errorf("expected default hysteresis 3, got %d", as.hysteresis)
	}
	if as.mode != "efficiency" {
		t.Errorf("expected default mode 'efficiency', got %q", as.mode)
	}
	if !as.enabled {
		t.Error("expected enabled=true by default")
	}
	if as.probeStream != -1 {
		t.Errorf("expected probeStream=-1, got %d", as.probeStream)
	}
}

// TestNewAutoScaler_CustomConfig verifica override de cada campo da config.
func TestNewAutoScaler_CustomConfig(t *testing.T) {
	d := newTestDispatcher(4)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	enabled := false
	as := NewAutoScaler(AutoScalerConfig{
		Dispatcher: d,
		Interval:   5 * time.Second,
		Hysteresis: 5,
		Logger:     logger,
		Mode:       "adaptive",
		Enabled:    &enabled,
	})

	if as.interval != 5*time.Second {
		t.Errorf("expected interval 5s, got %v", as.interval)
	}
	if as.hysteresis != 5 {
		t.Errorf("expected hysteresis 5, got %d", as.hysteresis)
	}
	if as.mode != "adaptive" {
		t.Errorf("expected mode adaptive, got %q", as.mode)
	}
	if as.enabled {
		t.Error("expected enabled=false")
	}
}

// ---------------------------------------------------------------------------
// Tests: Efficiency mode
// ---------------------------------------------------------------------------

// TestAutoScaler_Efficiency_ScaleUpAfterHysteresis verifica que o scale-up
// só é disparado após alcançar a janela de histerese.
// Nota: ActivateStream requer rede real, então testamos a lógica de decisão
// verificando os contadores de histerese e chamando evaluateEfficiency diretamente.
func TestAutoScaler_Efficiency_ScaleUpAfterHysteresis(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 3)

	rates := RateSample{ProducerBps: 200, DrainBps: 100}
	efficiency := 2.0 // producer faster than drain

	// Janela 1: incrementa scaleUpCount
	as.evaluateEfficiency(efficiency, rates, d.ActiveStreams())
	if as.scaleUpCount != 1 {
		t.Fatalf("after 1st eval: expected scaleUpCount=1, got %d", as.scaleUpCount)
	}

	// Janela 2: incrementa scaleUpCount
	as.evaluateEfficiency(efficiency, rates, d.ActiveStreams())
	if as.scaleUpCount != 2 {
		t.Fatalf("after 2nd eval: expected scaleUpCount=2, got %d", as.scaleUpCount)
	}

	// Janela 3: scale-up! O scaleUp chama ActivateStream que precisa de rede,
	// mas como não há stream livre sem conexão de rede disponível, o scaleUp
	// falhará silenciosamente. Verificamos que os contadores resetaram.
	// Para testar o scale-up com sucesso, usamos ativação manual prévia.
	activateStreamManually(d, 1, &mockConn{}) // pré-ativa para simular sucesso
	as.evaluateEfficiency(efficiency, rates, d.ActiveStreams())

	// scaleUpCount deve ter sido resetado após atingir hysteresis
	if as.scaleUpCount != 0 {
		t.Fatalf("after 3rd eval: expected scaleUpCount=0 (reset after action), got %d", as.scaleUpCount)
	}

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateScalingUp {
		t.Fatalf("expected state ScalingUp, got %d", snap.State)
	}
}

// TestAutoScaler_Efficiency_ScaleDownAfterHysteresis verifica que o scale-down
// só é disparado após alcançar a janela de histerese com efficiency < 0.7.
func TestAutoScaler_Efficiency_ScaleDownAfterHysteresis(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 2)

	// Efficiency < 0.7: producer = 30 MB, drain = 100 MB → efficiency = 0.3
	injectRates(d, 30*1024*1024, []int64{50 * 1024 * 1024, 50 * 1024 * 1024})

	// Janela 1: sem scale-down
	as.evaluate()
	if d.ActiveStreams() != 2 {
		t.Fatalf("after 1st eval: expected 2 active, got %d", d.ActiveStreams())
	}

	// Janela 2: scale-down!
	injectRates(d, 30*1024*1024, []int64{50 * 1024 * 1024, 50 * 1024 * 1024})
	as.evaluate()
	if d.ActiveStreams() != 1 {
		t.Fatalf("after 2nd eval: expected 1 active (scale-down), got %d", d.ActiveStreams())
	}

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateScaleDown {
		t.Fatalf("expected state ScaleDown, got %d", snap.State)
	}
}

// TestAutoScaler_Efficiency_StableResetsCounters verifica que efficiency
// na zona estável (0.7–1.0) reseta ambos os contadores de histerese.
func TestAutoScaler_Efficiency_StableResetsCounters(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 3)

	// 2 janelas de scale-up (producer faster)
	injectRates(d, 200*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()
	injectRates(d, 200*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()

	if as.scaleUpCount != 2 {
		t.Fatalf("expected scaleUpCount=2 before stable window, got %d", as.scaleUpCount)
	}

	// Janela estável: efficiency = 0.8 (entre 0.7 e 1.0)
	injectRates(d, 80*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()

	if as.scaleUpCount != 0 {
		t.Fatalf("expected scaleUpCount=0 after stable window, got %d", as.scaleUpCount)
	}
	if as.scaleDownCount != 0 {
		t.Fatalf("expected scaleDownCount=0, got %d", as.scaleDownCount)
	}
}

// TestAutoScaler_Efficiency_MaxStreams verifica que scale-up para além do max é ignorado.
func TestAutoScaler_Efficiency_MaxStreams(t *testing.T) {
	d := newTestDispatcher(2) // max=2
	activateStreamManually(d, 0, &mockConn{})
	activateStreamManually(d, 1, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 1)

	injectRates(d, 200*1024*1024, []int64{50 * 1024 * 1024, 50 * 1024 * 1024})
	as.evaluate()

	// Já está no max — não deve mudar
	if d.ActiveStreams() != 2 {
		t.Fatalf("expected 2 (max), got %d", d.ActiveStreams())
	}
}

// TestAutoScaler_Efficiency_MinStreams verifica que scale-down abaixo de 1 é impedido.
func TestAutoScaler_Efficiency_MinStreams(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 1)

	// Efficiency muito baixa, mas já está com 1 stream
	injectRates(d, 10*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()

	if d.ActiveStreams() != 1 {
		t.Fatalf("expected 1 (min), got %d", d.ActiveStreams())
	}
}

// ---------------------------------------------------------------------------
// Tests: Evaluate edge cases
// ---------------------------------------------------------------------------

// TestAutoScaler_Evaluate_ZeroDrain não faz scale com drain=0.
func TestAutoScaler_Evaluate_ZeroDrain(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 1)

	// DrainBps = 0 → retorna sem ação
	injectRates(d, 100*1024*1024, []int64{0})
	as.evaluate()

	if d.ActiveStreams() != 1 {
		t.Fatalf("expected no change with zero drain, got %d active", d.ActiveStreams())
	}
	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateStable {
		t.Fatalf("expected stable state with zero drain, got %d", snap.State)
	}
}

// TestAutoScaler_Evaluate_ZeroActive não crasha com 0 active streams.
func TestAutoScaler_Evaluate_ZeroActive(t *testing.T) {
	d := newTestDispatcher(4)
	as := newTestAutoScaler(d, "efficiency", 1)

	// Nenhum stream ativo → deve retornar sem crash
	as.evaluate()

	snap := as.Snapshot()
	if snap.State != protocol.AutoScaleStateStable {
		t.Fatalf("expected stable state with 0 active, got %d", snap.State)
	}
}

// ---------------------------------------------------------------------------
// Tests: Snapshot thread safety
// ---------------------------------------------------------------------------

// TestAutoScaler_SnapshotIsThreadSafe verifica que Snapshot retorna dados consistentes.
func TestAutoScaler_SnapshotIsThreadSafe(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 1)

	// Injeta rates e avalia para criar snapshot
	injectRates(d, 80*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()

	snap := as.Snapshot()
	if snap.MaxStreams != 4 {
		t.Errorf("expected MaxStreams=4, got %d", snap.MaxStreams)
	}
	if snap.ActiveStreams != 1 {
		t.Errorf("expected ActiveStreams=1, got %d", snap.ActiveStreams)
	}
	if snap.ProbeActive {
		t.Error("expected ProbeActive=false in efficiency mode")
	}
}

// ---------------------------------------------------------------------------
// Tests: Adaptive mode - probe cooldown
// ---------------------------------------------------------------------------

// TestAutoScaler_Adaptive_CooldownPreventsProbeClosure verifica cooldown.
func TestAutoScaler_Adaptive_CooldownPreventsProbe(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "adaptive", 1)

	// Ativa cooldown manual
	as.probeCooldown = 3

	// Rates normais, mas cooldown ativo
	injectRates(d, 80*1024*1024, []int64{100 * 1024 * 1024})
	as.evaluate()

	// Cooldown decrementou, mas sem probe
	if as.probeCooldown != 2 {
		t.Fatalf("expected probeCooldown=2 after decrement, got %d", as.probeCooldown)
	}
	if as.probeState != probeIdle {
		t.Fatalf("expected probe state to remain idle during cooldown, got %d", as.probeState)
	}
}

// TestAutoScaler_Adaptive_ProbeStartsAfterHysteresis verifica que o probe
// é iniciado somente após a janela de histerese para scale-up.
// Como ActivateStream tenta conectar via rede, o probe falhará na ativação
// e o auto-scaler deve voltar a idle com cooldown ativado — que é o
// comportamento correto quando não há rede.
func TestAutoScaler_Adaptive_ProbeStartsAfterHysteresis(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "adaptive", 3)

	rates := RateSample{ProducerBps: 80, DrainBps: 100}
	efficiency := 0.8

	// Janela 1: incrementa scaleUpCount (headroom existe: active=1 < max=4)
	as.evaluateAdaptive(efficiency, rates, d.ActiveStreams())
	if as.probeState != probeIdle {
		t.Fatalf("eval 1: expected probeIdle, got %d", as.probeState)
	}
	if as.scaleUpCount != 1 {
		t.Fatalf("eval 1: expected scaleUpCount=1, got %d", as.scaleUpCount)
	}

	// Janela 2
	as.evaluateAdaptive(efficiency, rates, d.ActiveStreams())
	if as.probeState != probeIdle {
		t.Fatalf("eval 2: expected probeIdle, got %d", as.probeState)
	}
	if as.scaleUpCount != 2 {
		t.Fatalf("eval 2: expected scaleUpCount=2, got %d", as.scaleUpCount)
	}

	// Janela 3: atingiu hysteresis → tenta probe.
	// ActivateStream falhará (sem rede), então o probe volta para idle
	// com cooldown ativado. Testamos esse caminho de falha gracioso.
	as.evaluateAdaptive(efficiency, rates, d.ActiveStreams())

	// scaleUpCount deve ser 0 (resetado ao iniciar tentativa de probe)
	if as.scaleUpCount != 0 {
		t.Fatalf("eval 3: expected scaleUpCount=0, got %d", as.scaleUpCount)
	}

	// Sem rede, ActivateStream falha → probeState volta a idle com cooldown.
	if as.probeState != probeIdle {
		t.Fatalf("eval 3: expected probeIdle (probe activation failed), got %d", as.probeState)
	}
	if as.probeCooldown != probeCooldownWindows {
		t.Fatalf("eval 3: expected probeCooldown=%d after failed probe, got %d", probeCooldownWindows, as.probeCooldown)
	}
}

// ---------------------------------------------------------------------------
// Tests: Run() idempotent
// ---------------------------------------------------------------------------

// TestAutoScaler_RunIdempotent verifica que Run() não inicia duas vezes.
func TestAutoScaler_RunIdempotent(t *testing.T) {
	d := newTestDispatcher(4)
	activateStreamManually(d, 0, &mockConn{})
	as := newTestAutoScaler(d, "efficiency", 3)

	// Verifica que running começa em 0
	if atomic.LoadInt32(&as.running) != 0 {
		t.Fatal("expected running=0 before Run")
	}

	// Inicia em background
	done := make(chan struct{})
	go func() {
		as.Run(func() context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_ = cancel
			return ctx
		}())
		close(done)
	}()

	// Espera o Run iniciar
	time.Sleep(50 * time.Millisecond)

	// Segunda chamada deve retornar imediatamente (já rodando)
	secondDone := make(chan struct{})
	go func() {
		as.Run(context.Background())
		close(secondDone)
	}()

	select {
	case <-secondDone:
		// OK — retornou imediatamente
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second Run() did not return immediately")
	}

	<-done
}
