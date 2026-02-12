package agent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"
)

// Scheduler gerencia a execução periódica de backups via cron expression.
type Scheduler struct {
	cron     *cron.Cron
	logger   *slog.Logger
	backupFn func(ctx context.Context) error
	mu       sync.Mutex // garante apenas um backup por vez
	running  bool
}

// NewScheduler cria um Scheduler com a expressão cron fornecida.
func NewScheduler(schedule string, logger *slog.Logger, fn func(ctx context.Context) error) (*Scheduler, error) {
	s := &Scheduler{
		logger:   logger,
		backupFn: fn,
	}

	c := cron.New(cron.WithLogger(cron.VerbosePrintfLogger(slog.NewLogLogger(logger.Handler(), slog.LevelDebug))))
	if _, err := c.AddFunc(schedule, s.execute); err != nil {
		return nil, err
	}

	s.cron = c
	return s, nil
}

// Start inicia o scheduler.
func (s *Scheduler) Start() {
	s.logger.Info("scheduler started")
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

func (s *Scheduler) execute() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		s.logger.Warn("backup already running, skipping scheduled execution")
		return
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.logger.Info("scheduled backup triggered")
	if err := s.backupFn(context.Background()); err != nil {
		s.logger.Error("backup failed", "error", err)
	}
}
