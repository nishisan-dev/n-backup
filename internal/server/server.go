// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// Package server implementa o servidor de backup (nbackup-server).
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
)

// Run inicia o servidor de backup e bloqueia até o context ser cancelado.
func Run(ctx context.Context, cfg *config.ServerConfig, logger *slog.Logger) error {
	// Configura TLS
	tlsCfg, err := pki.NewServerTLSConfig(cfg.TLS.CACert, cfg.TLS.ServerCert, cfg.TLS.ServerKey)
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	// Listener TLS
	ln, err := tls.Listen("tcp", cfg.Server.Listen, tlsCfg)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Server.Listen, err)
	}
	defer ln.Close()

	logger.Info("server listening", "address", cfg.Server.Listen)

	// Locks por agent (para prevenir backups simultâneos do mesmo agent)
	locks := &sync.Map{}
	handler := NewHandler(cfg, logger, locks)

	// Goroutine para fechar o listener quando o context for cancelado
	go func() {
		<-ctx.Done()
		logger.Info("shutting down server")
		ln.Close()
	}()

	// Accept loop
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				logger.Info("server shutdown complete")
				return nil
			default:
				logger.Error("accepting connection", "error", err)
				continue
			}
		}

		go handler.HandleConnection(ctx, conn)
	}
}

// RunWithListener inicia o servidor com um listener já existente (para testes).
func RunWithListener(ctx context.Context, ln net.Listener, cfg *config.ServerConfig, logger *slog.Logger) error {
	locks := &sync.Map{}
	handler := NewHandler(cfg, logger, locks)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				logger.Error("accepting connection", "error", err)
				continue
			}
		}

		go handler.HandleConnection(ctx, conn)
	}
}