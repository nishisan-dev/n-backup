// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"
)

const statsInterval = 5 * time.Minute

// jobSnapshot captura o estado de um job para o log estruturado.
type jobSnapshot struct {
	Name           string  `json:"name"`
	Schedule       string  `json:"schedule"`
	Parallels      int     `json:"parallels"`
	Status         string  `json:"status"`
	ActiveStreams  int     `json:"active_streams,omitempty"`
	MaxStreams     int     `json:"max_streams,omitempty"`
	LastStatus     string  `json:"last_status,omitempty"`
	LastDurationS  float64 `json:"last_duration_s,omitempty"`
	LastBytes      int64   `json:"last_bytes,omitempty"`
	LastObjects    int64   `json:"last_objects,omitempty"`
	LastAt         string  `json:"last_at,omitempty"`
	HandshakeRttMs float64 `json:"handshake_rtt_ms,omitempty"`
}

// StatsReporter emite métricas periódicas do daemon no log.
type StatsReporter struct {
	scheduler *Scheduler
	logger    *slog.Logger
	startTime time.Time
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewStatsReporter cria um StatsReporter que loga métricas a cada 5 minutos.
func NewStatsReporter(scheduler *Scheduler, logger *slog.Logger) *StatsReporter {
	return &StatsReporter{
		scheduler: scheduler,
		logger:    logger,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
}

// Start inicia a goroutine de reporting periódico.
func (sr *StatsReporter) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	sr.cancel = cancel

	go func() {
		defer close(sr.done)
		ticker := time.NewTicker(statsInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				sr.report()
			case <-ctx.Done():
				return
			}
		}
	}()

	sr.logger.Info("stats reporter started", "interval", statsInterval)
}

// Stop para o reporter e aguarda a goroutine terminar.
func (sr *StatsReporter) Stop() {
	if sr.cancel != nil {
		sr.cancel()
	}
	<-sr.done
	sr.logger.Info("stats reporter stopped")
}

func (sr *StatsReporter) report() {
	jobs := sr.scheduler.Jobs()
	uptime := time.Since(sr.startTime).Seconds()

	var runningCount int
	snapshots := make([]jobSnapshot, 0, len(jobs))

	for _, job := range jobs {
		snap := jobSnapshot{
			Name:      job.Entry.Name,
			Schedule:  job.Entry.Schedule,
			Parallels: job.Entry.Parallels,
		}

		job.mu.Lock()
		isRunning := job.running
		lastResult := job.LastResult
		job.mu.Unlock()

		if isRunning {
			runningCount++
			snap.Status = "running"

			// Captura métricas de streams paralelos (atômicas, sem lock)
			activeStreams := int(atomic.LoadInt32(&job.ActiveStreams))
			maxStreams := int(atomic.LoadInt32(&job.MaxStreams))
			if maxStreams > 0 {
				snap.ActiveStreams = activeStreams
				snap.MaxStreams = maxStreams
			}
		} else {
			snap.Status = "idle"
		}

		if lastResult != nil {
			snap.LastStatus = lastResult.Status
			snap.LastDurationS = lastResult.DurationSeconds
			snap.LastBytes = lastResult.BytesTransferred
			snap.LastObjects = lastResult.ObjectsCount
			snap.LastAt = lastResult.Timestamp.Format(time.RFC3339)
			if lastResult.HandshakeRTT > 0 {
				snap.HandshakeRttMs = float64(lastResult.HandshakeRTT.Microseconds()) / 1000.0
			}
		}

		snapshots = append(snapshots, snap)
	}

	// Serializa jobs como JSON para log estruturado
	jobsJSON, _ := json.Marshal(snapshots)

	// Encontrar próximo agendamento
	entries := sr.scheduler.cron.Entries()
	var nextTime time.Time
	var nextJobName string
	now := time.Now()

	for i, cronEntry := range entries {
		next := cronEntry.Next
		if next.After(now) && (nextTime.IsZero() || next.Before(nextTime)) {
			nextTime = next
			if i < len(jobs) {
				nextJobName = jobs[i].Entry.Name
			}
		}
	}

	attrs := []any{
		"uptime_seconds", int64(uptime),
		"jobs_total", len(jobs),
		"jobs_running", runningCount,
	}

	if !nextTime.IsZero() {
		attrs = append(attrs,
			"next_scheduled_name", nextJobName,
			"next_scheduled_at", nextTime.Format(time.RFC3339),
		)
	}

	attrs = append(attrs, "jobs", json.RawMessage(jobsJSON))

	sr.logger.Info("daemon stats", attrs...)
}
