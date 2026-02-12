package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nishisan-dev/n-backup/internal/agent"
	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/logging"
)

func main() {
	// Subcomando "health" detectado via os.Args
	if len(os.Args) >= 3 && os.Args[1] == "health" {
		runHealthCheck(os.Args[2])
		return
	}

	configPath := flag.String("config", "/etc/nbackup/agent.yaml", "path to agent config file")
	once := flag.Bool("once", false, "run backup once and exit (no daemon)")
	flag.Parse()

	cfg, err := config.LoadAgentConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	logger := logging.NewLogger(cfg.Logging.Level, cfg.Logging.Format)

	if *once {
		// Execução única — roda todos os backups sequencialmente
		if err := agent.RunAllBackups(context.Background(), cfg, logger); err != nil {
			logger.Error("backup failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Daemon mode
	if err := agent.RunDaemon(cfg, logger); err != nil {
		logger.Error("daemon error", "error", err)
		os.Exit(1)
	}
}

func runHealthCheck(address string) {
	// Health check requer config para TLS
	configPath := "/etc/nbackup/agent.yaml"
	if len(os.Args) >= 4 {
		// Permite: nbackup-agent health <addr> --config <path>
		for i, arg := range os.Args {
			if arg == "--config" && i+1 < len(os.Args) {
				configPath = os.Args[i+1]
			}
		}
	}

	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config for health check: %v\n", err)
		os.Exit(1)
	}

	logger := logging.NewLogger(cfg.Logging.Level, cfg.Logging.Format)

	if err := agent.RunHealthCheck(address, cfg, logger); err != nil {
		fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
		os.Exit(1)
	}
}
