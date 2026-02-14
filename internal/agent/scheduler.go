// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/robfig/cron/v3"
)

// BackupJobResult armazena o resultado do último backup de um job.
type BackupJobResult struct {
	Status           string    `json:"status"` // "completed", "failed", "skipped"
	DurationSeconds  float64   `json:"duration_seconds"`
	BytesTransferred int64     `json:"bytes_transferred"`
	ObjectsCount     int64     `json:"objects_count"`
	Timestamp        time.Time `json:"timestamp"`
}

// BackupJob representa um job de backup com guard de execução.
type BackupJob struct {
	Entry      config.BackupEntry
	mu         sync.Mutex
	running    bool
	LastResult *BackupJobResult

	// Métricas de streams paralelos (atualizadas atomicamente durante execução)
	ActiveStreams int32 // atomic — streams TCP ativos no momento
	MaxStreams    int32 // atomic — máximo de streams configurado para esta execução
}

// Scheduler gerencia N cron jobs independentes, um por backup entry.
type Scheduler struct {
	cron   *cron.Cron
	logger *slog.Logger
	jobs   []*BackupJob
	cfg    *config.AgentConfig
}

// NewScheduler cria um Scheduler com um cron job por backup entry.
func NewScheduler(cfg *config.AgentConfig, logger *slog.Logger, runFn func(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger, job *BackupJob) error) (*Scheduler, error) {
	s := &Scheduler{
		logger: logger,
		cfg:    cfg,
	}

	c := cron.New(cron.WithLogger(cron.VerbosePrintfLogger(slog.NewLogLogger(logger.Handler(), slog.LevelDebug))))

	for _, entry := range cfg.Backups {
		job := &BackupJob{Entry: entry}
		s.jobs = append(s.jobs, job)

		// Captura variáveis para closure
		jobRef := job
		entryRef := entry
		if _, err := c.AddFunc(entry.Schedule, func() {
			s.executeJob(jobRef, entryRef, runFn)
		}); err != nil {
			return nil, fmt.Errorf("adding cron job for backup %q: %w", entry.Name, err)
		}

		logger.Info("registered backup job",
			"backup", entry.Name,
			"storage", entry.Storage,
			"schedule", entry.Schedule,
			"parallels", entry.Parallels,
		)
	}

	s.cron = c
	return s, nil
}

// Start inicia o scheduler.
func (s *Scheduler) Start() {
	s.logger.Info("scheduler started", "jobs", len(s.jobs))
	s.cron.Start()
}

// Stop para o scheduler e aguarda jobs em andamento.
func (s *Scheduler) Stop(ctx context.Context) {
	s.logger.Info("scheduler stopping")
	stopCtx := s.cron.Stop()

	select {
	case <-stopCtx.Done():
		s.logger.Info("scheduler stopped gracefully")
	case <-ctx.Done():
		s.logger.Warn("scheduler stop timed out")
	}
}

// Jobs retorna os jobs registrados (para StatsReporter).
func (s *Scheduler) Jobs() []*BackupJob {
	return s.jobs
}

func (s *Scheduler) executeJob(job *BackupJob, entry config.BackupEntry, runFn func(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger, job *BackupJob) error) {
	entryLogger := s.logger.With("backup", entry.Name, "storage", entry.Storage)

	job.mu.Lock()
	if job.running {
		job.mu.Unlock()
		entryLogger.Warn("backup already running, skipping scheduled execution")
		job.LastResult = &BackupJobResult{
			Status:    "skipped",
			Timestamp: time.Now(),
		}
		return
	}
	job.running = true
	job.mu.Unlock()

	defer func() {
		job.mu.Lock()
		job.running = false
		job.mu.Unlock()
	}()

	entryLogger.Info("scheduled backup triggered")
	start := time.Now()

	// Inicializa métricas de streams antes da execução
	atomic.StoreInt32(&job.MaxStreams, int32(entry.Parallels))
	atomic.StoreInt32(&job.ActiveStreams, 0)

	err := runFn(context.Background(), s.cfg, entry, entryLogger, job)
	duration := time.Since(start)

	// Reseta métricas de streams após execução
	atomic.StoreInt32(&job.ActiveStreams, 0)
	atomic.StoreInt32(&job.MaxStreams, 0)

	if err != nil {
		entryLogger.Error("backup failed", "error", err, "duration", duration)
		job.LastResult = &BackupJobResult{
			Status:          "failed",
			DurationSeconds: duration.Seconds(),
			Timestamp:       time.Now(),
		}
	} else {
		entryLogger.Info("backup completed", "duration", duration)
		job.LastResult = &BackupJobResult{
			Status:          "completed",
			DurationSeconds: duration.Seconds(),
			Timestamp:       time.Now(),
		}
	}
}
