// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSessionLogger_Disabled(t *testing.T) {
	base := slog.New(slog.NewTextHandler(os.Stderr, nil))

	logger, closer, path, err := NewSessionLogger(base, "", "agent", "session-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer closer.Close()

	if logger != base {
		t.Error("expected base logger when sessionLogDir is empty")
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestNewSessionLogger_CreatesFileAndLogs(t *testing.T) {
	dir := t.TempDir()
	var baseBuf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&baseBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logger, closer, logPath, err := NewSessionLogger(base, dir, "test-agent", "session-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verifica que o diretório do agent foi criado
	agentDir := filepath.Join(dir, "test-agent")
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		t.Fatalf("agent dir not created: %s", agentDir)
	}

	// Verifica que o path retornado está correto
	expectedPath := filepath.Join(agentDir, "session-abc.log")
	if logPath != expectedPath {
		t.Errorf("expected path %q, got %q", expectedPath, logPath)
	}

	// Escreve um log
	logger.Info("test message", "key", "value")

	// Fecha o arquivo de sessão para garantir flush
	closer.Close()

	// Verifica que o log aparece no buffer do handler base
	if !strings.Contains(baseBuf.String(), "test message") {
		t.Errorf("log message not found in base handler output: %s", baseBuf.String())
	}

	// Verifica que o log aparece no arquivo de sessão
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading session log file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "test message") {
		t.Errorf("log message not found in session file: %s", content)
	}
	if !strings.Contains(content, `"key":"value"`) {
		t.Errorf("structured key not found in session file: %s", content)
	}
}

func TestNewSessionLogger_DebugInFileInfoInBase(t *testing.T) {
	dir := t.TempDir()

	// Base logger com nível INFO — não aceita DEBUG
	var baseBuf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&baseBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logger, closer, logPath, err := NewSessionLogger(base, dir, "agent", "sess-debug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Escreve log DEBUG
	logger.Debug("debug only message")
	logger.Info("info for both")

	closer.Close()

	// DEBUG NÃO deve aparecer no handler base (filtrado por nível INFO)
	if strings.Contains(baseBuf.String(), "debug only message") {
		t.Error("DEBUG message should not appear in base handler with INFO level")
	}
	// INFO DEVE aparecer no handler base
	if !strings.Contains(baseBuf.String(), "info for both") {
		t.Error("INFO message missing from base handler")
	}

	// Ambos DEVEM aparecer no arquivo de sessão (nível DEBUG)
	data, _ := os.ReadFile(logPath)
	content := string(data)
	if !strings.Contains(content, "debug only message") {
		t.Errorf("DEBUG message missing from session file: %s", content)
	}
	if !strings.Contains(content, "info for both") {
		t.Errorf("INFO message missing from session file: %s", content)
	}
}

func TestRemoveSessionLog(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	os.MkdirAll(agentDir, 0755)

	logPath := filepath.Join(agentDir, "session-to-remove.log")
	os.WriteFile(logPath, []byte("test"), 0644)

	// Verifica que o arquivo existe
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Fatal("setup failed: log file not created")
	}

	RemoveSessionLog(dir, "agent", "session-to-remove")

	// Verifica que o arquivo foi removido
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("session log file should have been removed")
	}
}

func TestRemoveSessionLog_NoOpWhenEmpty(t *testing.T) {
	// Não deve panic ou erro quando sessionLogDir é vazio
	RemoveSessionLog("", "agent", "session")
}

func TestRemoveSessionLog_NoOpWhenFileMissing(t *testing.T) {
	// Não deve panic ou erro quando o arquivo não existe
	RemoveSessionLog(t.TempDir(), "agent", "nonexistent-session")
}

func TestNewSessionLogger_WithAttrs(t *testing.T) {
	dir := t.TempDir()
	var baseBuf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&baseBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logger, closer, logPath, err := NewSessionLogger(base, dir, "agent", "sess-attrs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Adiciona attrs (como o handler.go faz com logger.With("session", sessionID))
	enriched := logger.With("session", "sess-attrs", "mode", "parallel")
	enriched.Info("enriched message")

	closer.Close()

	// Verifica que os attrs aparecem em ambos
	if !strings.Contains(baseBuf.String(), "sess-attrs") {
		t.Error("session attr missing from base handler")
	}

	data, _ := os.ReadFile(logPath)
	content := string(data)
	if !strings.Contains(content, "sess-attrs") {
		t.Errorf("session attr missing from session file: %s", content)
	}
	if !strings.Contains(content, "parallel") {
		t.Errorf("mode attr missing from session file: %s", content)
	}
}
