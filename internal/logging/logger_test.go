package logging

import (
	"testing"
)

func TestNewLogger_JSONFormat(t *testing.T) {
	logger := NewLogger("info", "json")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	logger := NewLogger("debug", "text")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_DefaultFormat(t *testing.T) {
	// Formato desconhecido deve cair no default (JSON)
	logger := NewLogger("info", "unknown")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_AllLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "warning", "error", "unknown"}
	for _, level := range levels {
		logger := NewLogger(level, "json")
		if logger == nil {
			t.Errorf("expected non-nil logger for level %q", level)
		}
	}
}
