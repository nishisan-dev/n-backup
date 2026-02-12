// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ProgressReporter exibe progresso de backup no terminal.
// Mostra barra, bytes, velocidade, objetos, elapsed, ETA e retries.
type ProgressReporter struct {
	name string

	// Contadores atômicos
	bytesWritten atomic.Int64
	objectsDone  atomic.Int64
	retries      atomic.Int32

	// Totais estimados (do pré-scan)
	totalBytes   int64
	totalObjects int64

	startTime time.Time
	done      chan struct{}
}

// NewProgressReporter cria um reporter e inicia o ticker de renderização.
func NewProgressReporter(name string, totalBytes, totalObjects int64) *ProgressReporter {
	p := &ProgressReporter{
		name:         name,
		totalBytes:   totalBytes,
		totalObjects: totalObjects,
		startTime:    time.Now(),
		done:         make(chan struct{}),
	}
	go p.renderLoop()
	return p
}

// AddBytes registra bytes escritos (chamado pelo pipeline de streaming).
func (p *ProgressReporter) AddBytes(n int64) {
	p.bytesWritten.Add(n)
}

// AddObject registra um objeto processado (arquivo/dir adicionado ao tar).
func (p *ProgressReporter) AddObject() {
	p.objectsDone.Add(1)
}

// AddRetry registra uma tentativa de retry/resume.
func (p *ProgressReporter) AddRetry() {
	p.retries.Add(1)
}

// Stop para o ticker e imprime a linha final.
func (p *ProgressReporter) Stop() {
	close(p.done)
	p.render(true)
}

// renderLoop atualiza o terminal a cada 500ms.
func (p *ProgressReporter) renderLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.render(false)
		}
	}
}

// render desenha a barra de progresso no stderr.
func (p *ProgressReporter) render(final bool) {
	bytes := p.bytesWritten.Load()
	objects := p.objectsDone.Load()
	retries := p.retries.Load()
	elapsed := time.Since(p.startTime)

	// Velocidade
	elapsedSec := elapsed.Seconds()
	var speed float64
	var objsPerSec float64
	if elapsedSec > 0.1 {
		speed = float64(bytes) / elapsedSec
		objsPerSec = float64(objects) / elapsedSec
	}

	// Barra de progresso (30 chars)
	barWidth := 30
	var bar string
	var pct float64
	if p.totalBytes > 0 {
		pct = float64(bytes) / float64(p.totalBytes)
		if pct > 1.0 {
			pct = 1.0 // compressão pode levar a menos bytes que raw
		}
		filled := int(pct * float64(barWidth))
		if filled > barWidth {
			filled = barWidth
		}
		bar = strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	} else {
		// Sem total — spinner simples
		pos := int(elapsed.Seconds()*2) % barWidth
		bar = strings.Repeat("░", pos) + "█" + strings.Repeat("░", barWidth-pos-1)
	}

	// ETA
	eta := "∞"
	if p.totalBytes > 0 && speed > 0 && bytes > 0 {
		remaining := float64(p.totalBytes) - float64(bytes)
		if remaining < 0 {
			remaining = 0
		}
		etaSec := remaining / speed
		eta = formatDuration(time.Duration(etaSec * float64(time.Second)))
	}

	// Formata elapsed
	elapsedStr := formatDuration(elapsed)

	// Retries
	retriesStr := ""
	if retries > 0 {
		retriesStr = fmt.Sprintf("  │  retries: %d", retries)
	}

	// Formata bytes e velocidade
	bytesStr := formatBytes(bytes)
	speedStr := formatBytes(int64(speed)) + "/s"

	line := fmt.Sprintf("\r[%s] %s  %s  │  %s  │  %s objs (%s/s)  │  %s  │  ETA %s%s",
		p.name, bar, bytesStr, speedStr,
		formatNumber(objects), formatNumber(int64(objsPerSec)),
		elapsedStr, eta, retriesStr,
	)

	// Pad com espaços para limpar restos de linha anterior
	if len(line) < 120 {
		line += strings.Repeat(" ", 120-len(line))
	}

	if final {
		fmt.Fprintf(os.Stderr, "%s\n", line)
	} else {
		fmt.Fprint(os.Stderr, line)
	}
}

// formatBytes formata bytes em unidades legíveis.
func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatDuration formata duração como M:SS ou H:MM:SS.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatNumber formata número com separador de milhar.
func formatNumber(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	// Insere separador a cada 3 dígitos da direita
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
