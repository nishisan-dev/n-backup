// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"testing"
)

func TestParseDSCP_ValidNames(t *testing.T) {
	tests := []struct {
		name     string
		expected int
	}{
		{"EF", 46},
		{"ef", 46},
		{"AF41", 34},
		{"af41", 34},
		{"AF11", 10},
		{"AF43", 38},
		{"CS0", 0},
		{"CS1", 8},
		{"CS7", 56},
		{"  AF31  ", 26}, // com espaço
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := ParseDSCP(tt.name)
			if err != nil {
				t.Fatalf("ParseDSCP(%q) error: %v", tt.name, err)
			}
			if val != tt.expected {
				t.Errorf("ParseDSCP(%q) = %d, want %d", tt.name, val, tt.expected)
			}
		})
	}
}

func TestParseDSCP_Empty(t *testing.T) {
	val, err := ParseDSCP("")
	if err != nil {
		t.Fatalf("ParseDSCP(\"\") error: %v", err)
	}
	if val != 0 {
		t.Errorf("ParseDSCP(\"\") = %d, want 0", val)
	}
}

func TestParseDSCP_Invalid(t *testing.T) {
	invalids := []string{"DSCP1", "XX", "AF50", "best-effort", "42"}

	for _, name := range invalids {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDSCP(name)
			if err == nil {
				t.Errorf("ParseDSCP(%q) expected error, got nil", name)
			}
		})
	}
}
