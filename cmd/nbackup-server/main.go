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
	"strconv"
	"strings"
	"syscall"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/logging"
	"github.com/nishisan-dev/n-backup/internal/server"
)

func main() {
	// Subcomando "sync-storage" detectado via os.Args
	if len(os.Args) >= 2 && os.Args[1] == "sync-storage" {
		runSyncStorage(os.Args[2:])
		return
	}

	configPath := flag.String("config", "/etc/nbackup/server.yaml", "path to server config file")
	flag.Parse()

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	logger, logCloser := logging.NewLogger(cfg.Logging.Level, cfg.Logging.Format, cfg.Logging.File)
	defer logCloser.Close()

	cfg.WarnDeprecated(logger)

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

// runSyncStorage envia SIGUSR1 ao daemon para triggerar sync retroativa.
//
// Uso:
//
//	nbackup-server sync-storage --pid <PID>
//	nbackup-server sync-storage --pid-file <path>
func runSyncStorage(args []string) {
	fs := flag.NewFlagSet("sync-storage", flag.ExitOnError)
	pidFlag := fs.Int("pid", 0, "PID of the running nbackup-server daemon")
	pidFileFlag := fs.String("pid-file", "", "path to PID file of the running daemon")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: nbackup-server sync-storage [--pid <PID> | --pid-file <path>]\n\n")
		fmt.Fprintf(os.Stderr, "Sends SIGUSR1 to the running nbackup-server daemon to trigger\n")
		fmt.Fprintf(os.Stderr, "retroactive storage sync with Object Storage (mode: sync).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	pid := *pidFlag

	// Resolve PID from file if --pid-file is provided
	if pid == 0 && *pidFileFlag != "" {
		data, err := os.ReadFile(*pidFileFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading PID file %s: %v\n", *pidFileFlag, err)
			os.Exit(1)
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing PID from file %s: %v\n", *pidFileFlag, err)
			os.Exit(1)
		}
		pid = parsed
	}

	if pid == 0 {
		fmt.Fprintf(os.Stderr, "Error: --pid or --pid-file is required\n\n")
		fs.Usage()
		os.Exit(1)
	}

	// Verifica se o processo existe
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding process %d: %v\n", pid, err)
		os.Exit(1)
	}

	// Envia SIGUSR1 ao daemon
	if err := process.Signal(syscall.SIGUSR1); err != nil {
		fmt.Fprintf(os.Stderr, "Error sending SIGUSR1 to PID %d: %v\n", pid, err)
		os.Exit(1)
	}

	fmt.Printf("SIGUSR1 sent to PID %d — storage sync triggered.\n", pid)
	fmt.Println("Check daemon logs for sync progress and results.")
}
