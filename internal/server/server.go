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
	"net/http"
	"sync"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// sessionTTL é o tempo máximo que uma sessão parcial pode ficar ativa sem resume (1h).
const sessionTTL = 1 * time.Hour

// sessionCleanupInterval é o intervalo entre limpezas de sessões expiradas.
const sessionCleanupInterval = 5 * time.Minute

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
	sessions := &sync.Map{}
	handler := NewHandler(cfg, logger, locks, sessions)

	// Goroutine para cleanup de sessões expiradas
	go func() {
		ticker := time.NewTicker(sessionCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				CleanupExpiredSessions(sessions, sessionTTL, logger)
			}
		}
	}()

	// Web UI HTTP server (observabilidade)
	if cfg.WebUI.Enabled {
		startWebUI(ctx, cfg, handler, logger)
	}

	// Stats reporter — imprime métricas a cada 15s
	go handler.StartStatsReporter(ctx)

	// Goroutine para fechar o listener quando o context for cancelado
	go func() {
		<-ctx.Done()
		logger.Info("shutting down server")
		ln.Close()
	}()

	// Accept loop com backoff para prevenir hot loop em erros consecutivos
	consecutiveErrors := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				logger.Info("server shutdown complete")
				return nil
			default:
				consecutiveErrors++
				logger.Error("accepting connection", "error", err, "consecutive_errors", consecutiveErrors)
				if consecutiveErrors > 5 {
					delay := time.Duration(consecutiveErrors) * 100 * time.Millisecond
					if delay > 5*time.Second {
						delay = 5 * time.Second
					}
					time.Sleep(delay)
				}
				continue
			}
		}

		consecutiveErrors = 0
		go handler.HandleConnection(ctx, conn)
	}
}

// RunWithListener inicia o servidor com um listener já existente (para testes).
func RunWithListener(ctx context.Context, ln net.Listener, cfg *config.ServerConfig, logger *slog.Logger) error {
	locks := &sync.Map{}
	sessions := &sync.Map{}
	handler := NewHandler(cfg, logger, locks, sessions)

	// Cleanup goroutine
	go func() {
		ticker := time.NewTicker(sessionCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				CleanupExpiredSessions(sessions, sessionTTL, logger)
			}
		}
	}()

	// Web UI HTTP server (observabilidade)
	if cfg.WebUI.Enabled {
		startWebUI(ctx, cfg, handler, logger)
	}

	// Stats reporter
	go handler.StartStatsReporter(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	consecutiveErrors := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				consecutiveErrors++
				logger.Error("accepting connection", "error", err, "consecutive_errors", consecutiveErrors)
				if consecutiveErrors > 5 {
					delay := time.Duration(consecutiveErrors) * 100 * time.Millisecond
					if delay > 5*time.Second {
						delay = 5 * time.Second
					}
					time.Sleep(delay)
				}
				continue
			}
		}

		consecutiveErrors = 0
		go handler.HandleConnection(ctx, conn)
	}
}

// startWebUI inicia o listener HTTP da SPA de observabilidade em background.
// O server é encerrado gracefully quando o context é cancelado.
func startWebUI(ctx context.Context, cfg *config.ServerConfig, handler *Handler, logger *slog.Logger) {
	acl := observability.NewACL(cfg.WebUI.ParsedCIDRs)
	events := observability.NewEventRing(1000)
	handler.Events = events
	router := observability.NewRouter(handler, cfg, acl, events)

	webSrv := &http.Server{
		Addr:              cfg.WebUI.Listen,
		Handler:           router,
		ReadTimeout:       cfg.WebUI.ReadTimeout,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      cfg.WebUI.WriteTimeout,
		IdleTimeout:       cfg.WebUI.IdleTimeout,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	go func() {
		logger.Info("web UI listening", "address", cfg.WebUI.Listen)
		if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("web UI server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := webSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("web UI shutdown error", "error", err)
		}
		logger.Info("web UI shutdown complete")
	}()
}
