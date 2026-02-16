// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// --- readLineLimited unit tests ---

func TestReadLineLimited_ValidLine(t *testing.T) {
	input := "hello-world\n"
	br := bufio.NewReader(strings.NewReader(input))

	got, err := readLineLimited(br, maxLineLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello-world" {
		t.Errorf("expected %q, got %q", "hello-world", got)
	}
}

func TestReadLineLimited_EmptyLine(t *testing.T) {
	input := "\n"
	br := bufio.NewReader(strings.NewReader(input))

	got, err := readLineLimited(br, maxLineLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestReadLineLimited_ExactlyAtLimit(t *testing.T) {
	line := strings.Repeat("x", maxLineLength) + "\n"
	br := bufio.NewReader(strings.NewReader(line))

	got, err := readLineLimited(br, maxLineLength)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != maxLineLength {
		t.Errorf("expected length %d, got %d", maxLineLength, len(got))
	}
}

func TestReadLineLimited_ExceedsLimit(t *testing.T) {
	line := strings.Repeat("x", maxLineLength+10) + "\n"
	br := bufio.NewReader(strings.NewReader(line))

	_, err := readLineLimited(br, maxLineLength)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got: %v", err)
	}
}

func TestReadLineLimited_Truncated_EOF(t *testing.T) {
	// Sem '\n' no final — simula frame truncado
	input := "incomplete"
	br := bufio.NewReader(strings.NewReader(input))

	_, err := readLineLimited(br, maxLineLength)
	if err == nil {
		t.Fatal("expected error for truncated input")
	}
	if errors.Is(err, ErrLineTooLong) {
		t.Fatal("expected EOF-like error, not ErrLineTooLong")
	}
}

// --- ReadResume integration tests ---

func TestReadResume_Oversized_SessionID(t *testing.T) {
	var buf bytes.Buffer
	// Version
	buf.WriteByte(ProtocolVersion)
	// SessionID com mais de maxLineLength bytes
	buf.WriteString(strings.Repeat("A", maxLineLength+100))
	buf.WriteByte('\n')
	buf.WriteString("agent\n")
	buf.WriteString("storage\n")

	_, err := ReadResume(&buf)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong for oversized sessionID, got: %v", err)
	}
}

func TestReadResume_Oversized_AgentName(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(ProtocolVersion)
	buf.WriteString("valid-session-id\n")
	buf.WriteString(strings.Repeat("B", maxLineLength+100))
	buf.WriteByte('\n')
	buf.WriteString("storage\n")

	_, err := ReadResume(&buf)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong for oversized agentName, got: %v", err)
	}
}

func TestReadResume_Oversized_StorageName(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(ProtocolVersion)
	buf.WriteString("valid-session-id\n")
	buf.WriteString("agent\n")
	buf.WriteString(strings.Repeat("C", maxLineLength+100))
	buf.WriteByte('\n')

	_, err := ReadResume(&buf)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong for oversized storageName, got: %v", err)
	}
}

// --- ReadParallelJoin integration tests ---

func TestReadParallelJoin_Oversized_SessionID(t *testing.T) {
	var buf bytes.Buffer
	// Version
	buf.WriteByte(ProtocolVersion)
	// SessionID com mais de maxLineLength bytes
	buf.WriteString(strings.Repeat("D", maxLineLength+100))
	buf.WriteByte('\n')
	buf.WriteByte(0) // streamIndex

	_, err := ReadParallelJoin(&buf)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong for oversized sessionID, got: %v", err)
	}
}
