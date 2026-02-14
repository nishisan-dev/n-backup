// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogger_JSONFormat(t *testing.T) {
	logger, closer := NewLogger("info", "json", "")
	defer closer.Close()
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	logger, closer := NewLogger("debug", "text", "")
	defer closer.Close()
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_DefaultFormat(t *testing.T) {
	// Formato desconhecido deve cair no default (JSON)
	logger, closer := NewLogger("info", "unknown", "")
	defer closer.Close()
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_AllLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "warning", "error", "unknown"}
	for _, level := range levels {
		logger, closer := NewLogger(level, "json", "")
		defer closer.Close()
		if logger == nil {
			t.Errorf("expected non-nil logger for level %q", level)
		}
	}
}

func TestNewLogger_WithFileOutput(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	logger, closer := NewLogger("info", "json", logFile)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	// Escreve algo no log
	logger.Info("test message", "key", "value")

	// Fecha o closer para flush
	closer.Close()

	// Verifica que o arquivo foi criado e contém dados
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "test message") {
		t.Errorf("expected log file to contain 'test message', got: %s", content)
	}
	if !strings.Contains(content, "key") {
		t.Errorf("expected log file to contain 'key', got: %s", content)
	}
}

func TestNewLogger_WithFileOutput_InvalidPath(t *testing.T) {
	// Path inválido — deve logar warning em stderr e retornar logger funcional
	logger, closer := NewLogger("info", "json", "/nonexistent/dir/test.log")
	defer closer.Close()

	if logger == nil {
		t.Fatal("expected non-nil logger even with invalid file path")
	}

	// Logger deve funcionar (stdout only)
	logger.Info("still works")
}
