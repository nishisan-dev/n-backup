package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
)

// RunDaemon inicia o agent em modo daemon com scheduler cron.
// Bloqueia até receber SIGTERM ou SIGINT.
func RunDaemon(cfg *config.AgentConfig, logger *slog.Logger) error {
	logger.Info("starting daemon",
		"agent", cfg.Agent.Name,
		"schedule", cfg.Daemon.Schedule,
		"backups", len(cfg.Backups),
	)

	backupFn := func(ctx context.Context) error {
		return RunAllBackups(ctx, cfg, logger)
	}

	sched, err := NewScheduler(cfg.Daemon.Schedule, logger, backupFn)
	if err != nil {
		return fmt.Errorf("creating scheduler: %w", err)
	}

	sched.Start()

	// Aguarda signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	// Graceful shutdown com timeout de 30s
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sched.Stop(ctx)
	return nil
}

// RunAllBackups executa todos os blocos de backup sequencialmente com retry.
func RunAllBackups(ctx context.Context, cfg *config.AgentConfig, logger *slog.Logger) error {
	var firstErr error

	for _, entry := range cfg.Backups {
		entryLogger := logger.With("backup", entry.Name, "storage", entry.Storage)
		entryLogger.Info("starting backup entry")

		err := RunBackupWithRetry(ctx, cfg, entry, entryLogger)
		if err != nil {
			entryLogger.Error("backup entry failed", "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("backup %q failed: %w", entry.Name, err)
			}
			continue // Continua com os próximos backups mesmo se um falhar
		}

		entryLogger.Info("backup entry completed successfully")
	}

	return firstErr
}

// RunBackupWithRetry executa um backup entry com retry usando exponential backoff.
func RunBackupWithRetry(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger) error {
	var lastErr error

	for attempt := 0; attempt < cfg.Retry.MaxAttempts; attempt++ {
		if attempt > 0 {
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

		err := RunBackup(ctx, cfg, entry, logger)
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
	n, err := conn.Read(buf)
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
