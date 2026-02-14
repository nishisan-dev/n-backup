// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"testing"
	"time"
)

// computeETA replica a lógica pessimista do render para ser testável isoladamente.
// Retorna a duração estimada ou -1 se indeterminado (∞).
func computeETA(totalBytes, bytesWritten int64, speed float64, totalObjects, objectsDone int64, objsPerSec float64) time.Duration {
	var etaBytesSec, etaObjsSec float64

	if totalBytes > 0 && speed > 0 {
		rem := float64(totalBytes) - float64(bytesWritten)
		if rem < 0 {
			rem = 0
		}
		etaBytesSec = rem / speed
	}

	if totalObjects > 0 && objsPerSec > 0 {
		rem := float64(totalObjects) - float64(objectsDone)
		if rem < 0 {
			rem = 0
		}
		etaObjsSec = rem / objsPerSec
	}

	hasTotals := totalBytes > 0 || totalObjects > 0
	if hasTotals && (speed > 0 || objsPerSec > 0) {
		pessimistic := etaBytesSec
		if etaObjsSec > pessimistic {
			pessimistic = etaObjsSec
		}
		return time.Duration(pessimistic * float64(time.Second))
	}

	return -1 // indeterminado
}

func TestETA_PessimisticUsesObjects(t *testing.T) {
	// Cenário: bytes já completaram (remaining=0), mas objetos ainda restam.
	// ETA deve vir exclusivamente dos objetos.
	eta := computeETA(
		100, 100, // totalBytes, bytesWritten (100%)
		10.0,                  // speed (bytes/s)
		11_966_809, 6_867_432, // totalObjects, objectsDone
		145.0, // objsPerSec
	)

	if eta < 0 {
		t.Fatal("ETA should not be indeterminate when objects remain")
	}

	// ETA esperado: (11966809 - 6867432) / 145 ≈ 35168s ≈ 9h46m
	expectedSec := float64(11_966_809-6_867_432) / 145.0
	gotSec := eta.Seconds()

	// Tolerância de 1 segundo por arredondamento
	if gotSec < expectedSec-1 || gotSec > expectedSec+1 {
		t.Errorf("expected ETA ≈ %.0fs, got %.0fs", expectedSec, gotSec)
	}
}

func TestETA_PessimisticPicksLargest(t *testing.T) {
	// Cenário: ambos bytes e objetos restam. ETA deve ser o MAIOR dos dois.
	// ETA bytes = (1000 - 500) / 100 = 5s
	// ETA objs  = (2000 - 100) / 10  = 190s  ← maior
	eta := computeETA(
		1000, 500, // totalBytes, bytesWritten
		100.0,     // speed
		2000, 100, // totalObjects, objectsDone
		10.0, // objsPerSec
	)

	if eta < 0 {
		t.Fatal("ETA should not be indeterminate")
	}

	expectedSec := 190.0
	gotSec := eta.Seconds()

	if gotSec < expectedSec-1 || gotSec > expectedSec+1 {
		t.Errorf("expected ETA ≈ %.0fs (objects), got %.0fs", expectedSec, gotSec)
	}
}

func TestETA_BothComplete(t *testing.T) {
	// Cenário: bytes e objetos 100% completos → ETA = 0
	eta := computeETA(
		1000, 1000, // totalBytes, bytesWritten
		100.0,    // speed
		500, 500, // totalObjects, objectsDone
		50.0, // objsPerSec
	)

	if eta < 0 {
		t.Fatal("ETA should not be indeterminate when both are complete")
	}

	if eta != 0 {
		t.Errorf("expected ETA = 0, got %v", eta)
	}
}

func TestETA_IndeterminateWithoutRates(t *testing.T) {
	// Cenário: sem velocidade medida (warm-up) → ETA indeterminado
	eta := computeETA(
		1000, 100,
		0.0, // sem speed
		500, 50,
		0.0, // sem objsPerSec
	)

	if eta != -1 {
		t.Errorf("expected indeterminate ETA (-1), got %v", eta)
	}
}

func TestETA_IndeterminateWithoutTotals(t *testing.T) {
	// Cenário: prescan pendente (totals=0), mesmo com speed medida → ETA indeterminado
	// Bug anterior: mostrava 0:00 em vez de ∞
	eta := computeETA(
		0, 1024*1024, // totalBytes=0 (prescan não terminou), bytes transferidos
		13.5e6, // speed 13.5 MB/s
		0, 30,  // totalObjects=0 (prescan não terminou), 30 objetos feitos
		0.5, // objsPerSec
	)

	if eta != -1 {
		t.Errorf("expected indeterminate ETA (-1) when totals unknown, got %v", eta)
	}
}

func TestStreamsDisplay(t *testing.T) {
	// Cria reporter sem iniciar o renderLoop (usamos diretamente SetStreams)
	p := &ProgressReporter{
		name:           "test",
		startTime:      time.Now(),
		warmupDuration: 0,
		done:           make(chan struct{}),
	}

	// Sem streams configurados — maxStreams = 0
	if p.maxStreams.Load() != 0 {
		t.Fatal("expected maxStreams = 0 initially")
	}

	// Configura 4 streams, 1 ativo
	p.SetStreams(1, 4)
	if p.activeStreams.Load() != 1 {
		t.Errorf("expected activeStreams = 1, got %d", p.activeStreams.Load())
	}
	if p.maxStreams.Load() != 4 {
		t.Errorf("expected maxStreams = 4, got %d", p.maxStreams.Load())
	}

	// Atualiza para 3 ativos
	p.SetStreams(3, 4)
	if p.activeStreams.Load() != 3 {
		t.Errorf("expected activeStreams = 3, got %d", p.activeStreams.Load())
	}
}

func TestDispatcher_OnStreamChangeCallback(t *testing.T) {
	var calls []struct{ active, max int }

	cfg := DispatcherConfig{
		MaxStreams: 3,
		BufferSize: 1024,
		ChunkSize:  256,
		OnStreamChange: func(active, max int) {
			calls = append(calls, struct{ active, max int }{active, max})
		},
	}

	// PrimaryConn é nil — ok para este teste pois não ativamos senders
	d := NewDispatcher(cfg)
	defer d.Close()

	// NewDispatcher deve ter chamado callback com (1, 3)
	if len(calls) != 1 {
		t.Fatalf("expected 1 callback call from NewDispatcher, got %d", len(calls))
	}
	if calls[0].active != 1 || calls[0].max != 3 {
		t.Errorf("expected callback(1, 3), got callback(%d, %d)", calls[0].active, calls[0].max)
	}

	// DeactivateStream de stream inválido não deve chamar callback
	d.DeactivateStream(5)
	if len(calls) != 1 {
		t.Error("DeactivateStream with invalid index should not trigger callback")
	}
}
