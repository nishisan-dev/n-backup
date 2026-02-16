// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePathComponent_Valid(t *testing.T) {
	valid := []string{
		"my-agent",
		"agent_01",
		"backup-daily",
		"storage123",
		"AgentName",
		"a",
	}
	for _, name := range valid {
		if err := validatePathComponent(name, "test"); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}
}

func TestValidatePathComponent_RejectsPathTraversal(t *testing.T) {
	invalid := []string{
		"..",
		"../../../etc/passwd",
		"..secret",
	}
	for _, name := range invalid {
		if err := validatePathComponent(name, "test"); err == nil {
			t.Errorf("expected %q to be rejected (path traversal)", name)
		}
	}
}

func TestValidatePathComponent_RejectsPathSeparators(t *testing.T) {
	invalid := []string{
		"foo/bar",
		"foo\\bar",
		"/absolute",
		"nested/path/name",
	}
	for _, name := range invalid {
		if err := validatePathComponent(name, "test"); err == nil {
			t.Errorf("expected %q to be rejected (path separator)", name)
		}
	}
}

func TestValidatePathComponent_RejectsEmpty(t *testing.T) {
	if err := validatePathComponent("", "test"); err == nil {
		t.Error("expected empty string to be rejected")
	}
}

func TestValidatePathComponent_RejectsNullByte(t *testing.T) {
	if err := validatePathComponent("foo\x00bar", "test"); err == nil {
		t.Error("expected string with null byte to be rejected")
	}
}

func TestValidatePathComponent_RejectsDotPrefix(t *testing.T) {
	invalid := []string{
		".hidden",
		".config",
		".",
	}
	for _, name := range invalid {
		if err := validatePathComponent(name, "test"); err == nil {
			t.Errorf("expected %q to be rejected (dot prefix)", name)
		}
	}
}

func TestValidatePathComponent_RejectsLongName(t *testing.T) {
	long := strings.Repeat("x", maxPathComponentLength+1)
	if err := validatePathComponent(long, "test"); err == nil {
		t.Error("expected long name to be rejected")
	}
}

func TestValidatePathInBaseDir_Inside(t *testing.T) {
	base := "/data/backups"
	inside := filepath.Join(base, "agent1", "daily")
	if err := validatePathInBaseDir(base, inside); err != nil {
		t.Errorf("expected path inside base dir, got error: %v", err)
	}
}

func TestValidatePathInBaseDir_Outside(t *testing.T) {
	base := "/data/backups"
	outside := "/etc/passwd"
	if err := validatePathInBaseDir(base, outside); err == nil {
		t.Error("expected path outside base dir to be rejected")
	}
}

func TestValidatePathInBaseDir_TraversalAttempt(t *testing.T) {
	base := "/data/backups"
	traversal := filepath.Join(base, "..", "..", "etc", "passwd")
	if err := validatePathInBaseDir(base, traversal); err == nil {
		t.Error("expected traversal attempt to be rejected")
	}
}
