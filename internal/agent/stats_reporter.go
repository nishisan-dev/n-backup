// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"log/slog"
	"time"
)

const statsInterval = 5 * time.Minute

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
	for _, job := range jobs {
		job.mu.Lock()
		if job.running {
			runningCount++
		}
		job.mu.Unlock()
	}

	// Encontrar último backup concluído
	var lastResult *BackupJobResult
	var lastBackupName string
	var latestTime time.Time

	for _, job := range jobs {
		if job.LastResult != nil && job.LastResult.Timestamp.After(latestTime) {
			latestTime = job.LastResult.Timestamp
			lastResult = job.LastResult
			lastBackupName = job.Entry.Name
		}
	}

	attrs := []any{
		"uptime_seconds", int64(uptime),
		"jobs_total", len(jobs),
		"jobs_running", runningCount,
	}

	if lastResult != nil {
		attrs = append(attrs,
			"last_backup_name", lastBackupName,
			"last_backup_status", lastResult.Status,
			"last_backup_duration_seconds", lastResult.DurationSeconds,
			"last_backup_bytes", lastResult.BytesTransferred,
			"last_backup_objects", lastResult.ObjectsCount,
			"last_backup_at", lastResult.Timestamp.Format(time.RFC3339),
		)
	}

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

	if !nextTime.IsZero() {
		attrs = append(attrs,
			"next_scheduled_name", nextJobName,
			"next_scheduled_at", nextTime.Format(time.RFC3339),
		)
	}

	sr.logger.Info("daemon stats", attrs...)
}
