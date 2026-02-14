// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
)

// RunDaemon inicia o agent em modo daemon com um cron job por backup.
// Bloqueia até receber SIGTERM ou SIGINT.
// SIGHUP recarrega a configuração sem downtime (systemctl reload).
func RunDaemon(configPath string, cfg *config.AgentConfig, logger *slog.Logger) error {
	logger.Info("starting daemon",
		"agent", cfg.Agent.Name,
		"backups", len(cfg.Backups),
	)

	runFn := func(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, entryLogger *slog.Logger, job *BackupJob) error {
		return RunBackupWithRetry(ctx, cfg, entry, entryLogger, nil, job)
	}

	sched, err := NewScheduler(cfg, logger, runFn)
	if err != nil {
		return fmt.Errorf("creating scheduler: %w", err)
	}

	sched.Start()

	// Stats reporter — emite métricas a cada 5 minutos
	stats := NewStatsReporter(sched, logger)
	stats.Start()

	// Aguarda signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	for {
		sig := <-sigCh

		if sig == syscall.SIGHUP {
			logger.Info("received SIGHUP, reloading config", "path", configPath)

			newCfg, loadErr := config.LoadAgentConfig(configPath)
			if loadErr != nil {
				logger.Error("reload failed, keeping current config", "error", loadErr)
				continue
			}

			// Para scheduler e stats atuais
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			stats.Stop()
			sched.Stop(stopCtx)
			stopCancel()

			// Recria com nova config
			cfg = newCfg
			sched, err = NewScheduler(cfg, logger, runFn)
			if err != nil {
				logger.Error("failed to create scheduler after reload", "error", err)
				return fmt.Errorf("reload scheduler: %w", err)
			}
			sched.Start()
			stats = NewStatsReporter(sched, logger)
			stats.Start()

			logger.Info("config reloaded successfully",
				"agent", cfg.Agent.Name,
				"backups", len(cfg.Backups),
			)
			continue
		}

		// SIGTERM ou SIGINT — graceful shutdown
		logger.Info("received signal, shutting down", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stats.Stop()
		sched.Stop(ctx)
		cancel()
		return nil
	}
}

// RunAllBackups executa todos os blocos de backup sequencialmente com retry.
// Se showProgress for true, exibe barra de progresso no terminal.
func RunAllBackups(ctx context.Context, cfg *config.AgentConfig, showProgress bool, logger *slog.Logger) error {
	var firstErr error

	for _, entry := range cfg.Backups {
		entryLogger := logger.With("backup", entry.Name, "storage", entry.Storage)
		entryLogger.Info("starting backup entry")

		var progress *ProgressReporter
		if showProgress {
			sources := make([]string, len(entry.Sources))
			for i, s := range entry.Sources {
				sources[i] = s.Path
			}
			// Inicia reporter imediatamente em modo spinner (totais=0)
			progress = NewProgressReporter(entry.Name, 0, 0)
			// PreScan em background — atualiza totais quando terminar
			go func() {
				scanner := NewScanner(sources, entry.Exclude)
				stats, err := scanner.PreScan(ctx)
				if err != nil {
					entryLogger.Warn("pre-scan failed, progress bar will estimate", "error", err)
					return
				}
				entryLogger.Info("pre-scan complete",
					"files", stats.TotalObjects,
					"raw_bytes", stats.TotalBytes,
				)
				estimatedCompressed := stats.TotalBytes / 2
				if estimatedCompressed == 0 {
					estimatedCompressed = stats.TotalBytes
				}
				progress.SetTotals(estimatedCompressed, stats.TotalObjects)
			}()
		}

		err := RunBackupWithRetry(ctx, cfg, entry, entryLogger, progress, nil)

		if progress != nil {
			progress.Stop()
		}

		if err != nil {
			entryLogger.Error("backup entry failed", "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("backup %q failed: %w", entry.Name, err)
			}
			continue
		}

		entryLogger.Info("backup entry completed successfully")
	}

	return firstErr
}

// RunBackupWithRetry executa um backup entry com retry usando exponential backoff.
func RunBackupWithRetry(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger, progress *ProgressReporter, job *BackupJob) error {
	var lastErr error

	for attempt := 0; attempt < cfg.Retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if progress != nil {
				progress.AddRetry()
			}
			delay := calculateBackoff(attempt, cfg.Retry.InitialDelay, cfg.Retry.MaxDelay)
			logger.Info("retrying backup",
				"attempt", attempt+1,
				"delay", delay,
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := RunBackup(ctx, cfg, entry, logger, progress, job)
		if err == nil {
			return nil
		}

		lastErr = err
		logger.Warn("backup attempt failed",
			"attempt", attempt+1,
			"error", err,
		)
	}

	return fmt.Errorf("all %d backup attempts failed, last error: %w", cfg.Retry.MaxAttempts, lastErr)
}

// calculateBackoff calcula o delay com exponential backoff capped.
func calculateBackoff(attempt int, initialDelay, maxDelay time.Duration) time.Duration {
	delay := time.Duration(float64(initialDelay) * math.Pow(2, float64(attempt-1)))
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

// RunHealthCheck executa um health check contra o servidor.
func RunHealthCheck(address string, cfg *config.AgentConfig, logger *slog.Logger) error {
	tlsCfg, err := loadClientTLS(cfg)
	if err != nil {
		return err
	}

	// Extrai hostname para ServerName
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	tlsCfg.ServerName = host

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialWithContext(ctx, address, tlsCfg)
	if err != nil {
		return fmt.Errorf("connecting for health check: %w", err)
	}
	defer conn.Close()

	// Envia PING
	if _, err := conn.Write([]byte("PING")); err != nil {
		return fmt.Errorf("sending ping: %w", err)
	}

	// Lê resposta (status 1B + diskFree 8B + '\n' 1B = 10B)
	buf := make([]byte, 10)
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		return fmt.Errorf("reading health response: %w", err)
	}

	if n < 1 {
		return fmt.Errorf("empty health response")
	}

	status := buf[0]
	switch status {
	case 0x00:
		fmt.Println("Server status: READY")
	case 0x01:
		fmt.Println("Server status: BUSY")
	case 0x02:
		fmt.Println("Server status: LOW DISK")
	case 0x03:
		fmt.Println("Server status: MAINTENANCE")
	default:
		fmt.Printf("Server status: UNKNOWN (0x%02x)\n", status)
	}

	return nil
}

func loadClientTLS(cfg *config.AgentConfig) (*tls.Config, error) {
	return pki.NewClientTLSConfig(cfg.TLS.CACert, cfg.TLS.ClientCert, cfg.TLS.ClientKey)
}
