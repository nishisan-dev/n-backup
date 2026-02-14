// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger cria um slog.Logger configurado com o nível, formato e output especificados.
// Formatos suportados: "json" (default) e "text".
// Níveis suportados: "debug", "info" (default), "warn", "error".
// Se filePath não for vazio, grava logs em stdout + file (MultiWriter).
// Retorna o logger e um io.Closer que deve ser chamado no shutdown para fechar o arquivo.
// Se filePath for vazio, o Closer retornado é um no-op.
func NewLogger(level, format, filePath string) (*slog.Logger, io.Closer) {
	lvl := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}

	var w io.Writer = os.Stdout
	var closer io.Closer = io.NopCloser(strings.NewReader(""))

	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			// Se não conseguir abrir o arquivo, loga stderr e continua só com stdout
			fmt.Fprintf(os.Stderr, "WARNING: could not open log file %q: %v (logging to stdout only)\n", filePath, err)
		} else {
			w = io.MultiWriter(os.Stdout, f)
			closer = f
		}
	}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}

	return slog.New(handler), closer
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
