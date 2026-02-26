// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// fanOutHandler é um slog.Handler que despacha cada registro para dois handlers.
// Usado pelo SessionLogger para gravar simultaneamente no handler global e no
// arquivo de log dedicado da sessão.
type fanOutHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (h *fanOutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level) || h.secondary.Enabled(ctx, level)
}

func (h *fanOutHandler) Handle(ctx context.Context, r slog.Record) error {
	// Verifica Enabled() de cada handler individualmente antes de despachar.
	// Isso garante que registros DEBUG não são enviados ao handler primário
	// quando este aceita apenas INFO (ou superior).
	if h.primary.Enabled(ctx, r.Level) {
		if err := h.primary.Handle(ctx, r); err != nil {
			return err
		}
	}
	// Erros de escrita no arquivo de sessão não devem impedir o log global.
	if h.secondary.Enabled(ctx, r.Level) {
		_ = h.secondary.Handle(ctx, r)
	}
	return nil
}

func (h *fanOutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanOutHandler{
		primary:   h.primary.WithAttrs(attrs),
		secondary: h.secondary.WithAttrs(attrs),
	}
}

func (h *fanOutHandler) WithGroup(name string) slog.Handler {
	return &fanOutHandler{
		primary:   h.primary.WithGroup(name),
		secondary: h.secondary.WithGroup(name),
	}
}

// NewSessionLogger cria um logger que grava tanto no logger base (global) quanto
// em um arquivo dedicado para a sessão. O arquivo é criado em:
//
//	{sessionLogDir}/{agentName}/{sessionID}.log
//
// Retorna o logger enriched, um io.Closer para fechar o arquivo de sessão e o
// path absoluto do arquivo criado. O Closer DEVE ser chamado (defer) quando a
// sessão terminar.
//
// Se sessionLogDir for vazio, retorna o logger base sem modificações (no-op).
func NewSessionLogger(baseLogger *slog.Logger, sessionLogDir, agentName, sessionID string) (*slog.Logger, io.Closer, string, error) {
	if sessionLogDir == "" {
		return baseLogger, io.NopCloser(nil), "", nil
	}

	dir := filepath.Join(sessionLogDir, agentName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, "", fmt.Errorf("creating session log directory %s: %w", dir, err)
	}

	logPath := filepath.Join(dir, sessionID+".log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, "", fmt.Errorf("opening session log file %s: %w", logPath, err)
	}

	// Arquivo de sessão sempre usa JSON com nível DEBUG para captura máxima.
	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// Fan-out: despacha para o handler do logger base + handler do arquivo.
	combined := &fanOutHandler{
		primary:   baseLogger.Handler(),
		secondary: fileHandler,
	}

	return slog.New(combined), f, logPath, nil
}

// RemoveSessionLog remove o arquivo de log de uma sessão finalizada com sucesso.
// É no-op se sessionLogDir for vazio ou o arquivo não existir.
func RemoveSessionLog(sessionLogDir, agentName, sessionID string) {
	if sessionLogDir == "" {
		return
	}
	logPath := filepath.Join(sessionLogDir, agentName, sessionID+".log")
	os.Remove(logPath)
}
