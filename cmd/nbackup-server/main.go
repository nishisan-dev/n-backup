// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/logging"
	"github.com/nishisan-dev/n-backup/internal/server"
)

func main() {
	configPath := flag.String("config", "/etc/nbackup/server.yaml", "path to server config file")
	flag.Parse()

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	logger := logging.NewLogger(cfg.Logging.Level, cfg.Logging.Format)

	// Context com cancelamento via signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}