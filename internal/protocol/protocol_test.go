// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestHandshake_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	agentName := "web-server-01"
	storageName := "scripts"
	backupName := "app"

	if err := WriteHandshake(&buf, agentName, storageName, backupName); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	hs, err := ReadHandshake(&buf)
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}

	if hs.Version != ProtocolVersion {
		t.Errorf("expected version %d, got %d", ProtocolVersion, hs.Version)
	}
	if hs.AgentName != agentName {
		t.Errorf("expected agent name %q, got %q", agentName, hs.AgentName)
	}
	if hs.StorageName != storageName {
		t.Errorf("expected storage name %q, got %q", storageName, hs.StorageName)
	}
	if hs.BackupName != backupName {
		t.Errorf("expected backup name %q, got %q", backupName, hs.BackupName)
	}
}

func TestACK_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		status    byte
		message   string
		sessionID string
	}{
		{"GO with session", StatusGo, "", "abc-123"},
		{"FULL with message", StatusFull, "disk is full", ""},
		{"BUSY with message", StatusBusy, "backup in progress", ""},
		{"REJECT with message", StatusReject, "agent not authorized", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			if err := WriteACK(&buf, tt.status, tt.message, tt.sessionID); err != nil {
				t.Fatalf("WriteACK: %v", err)
			}

			ack, err := ReadACK(&buf)
			if err != nil {
				t.Fatalf("ReadACK: %v", err)
			}

			if ack.Status != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, ack.Status)
			}
			if ack.Message != tt.message {
				t.Errorf("expected message %q, got %q", tt.message, ack.Message)
			}
			if ack.SessionID != tt.sessionID {
				t.Errorf("expected sessionID %q, got %q", tt.sessionID, ack.SessionID)
			}
		})
	}
}

func TestTrailer_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	checksum := sha256.Sum256([]byte("test data"))
	size := uint64(12345)

	if err := WriteTrailer(&buf, checksum, size); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	trailer, err := ReadTrailer(&buf)
	if err != nil {
		t.Fatalf("ReadTrailer: %v", err)
	}

	if trailer.Checksum != checksum {
		t.Errorf("checksum mismatch")
	}
	if trailer.Size != size {
		t.Errorf("expected size %d, got %d", size, trailer.Size)
	}
}

func TestFinalACK_RoundTrip(t *testing.T) {
	statuses := []byte{FinalStatusOK, FinalStatusChecksumMismatch, FinalStatusWriteError}

	for _, status := range statuses {
		var buf bytes.Buffer

		if err := WriteFinalACK(&buf, status); err != nil {
			t.Fatalf("WriteFinalACK: %v", err)
		}

		ack, err := ReadFinalACK(&buf)
		if err != nil {
			t.Fatalf("ReadFinalACK: %v", err)
		}

		if ack.Status != status {
			t.Errorf("expected status %d, got %d", status, ack.Status)
		}
	}
}

func TestHealthCheck_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	if err := WritePing(&buf); err != nil {
		t.Fatalf("WritePing: %v", err)
	}

	if err := ReadPing(&buf); err != nil {
		t.Fatalf("ReadPing: %v", err)
	}

	// Health response
	var buf2 bytes.Buffer
	diskFree := uint64(1024 * 1024 * 1024 * 50) // 50 GB

	if err := WriteHealthResponse(&buf2, HealthStatusReady, diskFree); err != nil {
		t.Fatalf("WriteHealthResponse: %v", err)
	}

	resp, err := ReadHealthResponse(&buf2)
	if err != nil {
		t.Fatalf("ReadHealthResponse: %v", err)
	}

	if resp.Status != HealthStatusReady {
		t.Errorf("expected status %d, got %d", HealthStatusReady, resp.Status)
	}
	if resp.DiskFree != diskFree {
		t.Errorf("expected disk free %d, got %d", diskFree, resp.DiskFree)
	}
}

func TestHandshake_InvalidMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("XXXX")) // magic errado
	buf.WriteByte(ProtocolVersion)
	buf.Write([]byte("agent-name\n"))
	buf.Write([]byte("storage-name\n"))
	buf.Write([]byte("backup-name\n"))

	_, err := ReadHandshake(&buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestHandshake_InvalidVersion(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(MagicHandshake[:])
	buf.WriteByte(0xFF) // versão inválida
	buf.Write([]byte("agent-name\n"))
	buf.Write([]byte("storage-name\n"))
	buf.Write([]byte("backup-name\n"))

	_, err := ReadHandshake(&buf)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestTrailer_InvalidMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("FAIL"))
	buf.Write(make([]byte, 40)) // checksum + size

	_, err := ReadTrailer(&buf)
	if err == nil {
		t.Fatal("expected error for invalid trailer magic")
	}
}

func TestPing_InvalidMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("NOPE"))

	err := ReadPing(&buf)
	if err == nil {
		t.Fatal("expected error for invalid ping magic")
	}
}

func TestHandshake_Truncated(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("NB")) // apenas 2 bytes, magic incompleto

	_, err := ReadHandshake(&buf)
	if err == nil {
		t.Fatal("expected error for truncated handshake")
	}
}

func TestTrailer_Truncated(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(MagicTrailer[:])
	buf.Write(make([]byte, 10)) // checksum incompleto (precisa de 40 bytes)

	_, err := ReadTrailer(&buf)
	if err == nil {
		t.Fatal("expected error for truncated trailer")
	}
}

func TestHandshake_FrameSize(t *testing.T) {
	// Verifica que o overhead é mínimo conforme a spec
	var buf bytes.Buffer
	agentName := "web-server-01"
	storageName := "scripts"
	backupName := "app"

	if err := WriteHandshake(&buf, agentName, storageName, backupName); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	// Magic(4) + Version(1) + AgentName(14) + Delimiter(1) + StorageName(7) + Delimiter(1) + BackupName(3) + Delimiter(1) = 32 bytes
	expected := 4 + 1 + len(agentName) + 1 + len(storageName) + 1 + len(backupName) + 1
	if buf.Len() != expected {
		t.Errorf("expected handshake size %d, got %d", expected, buf.Len())
	}
}

func TestTrailer_FrameSize(t *testing.T) {
	var buf bytes.Buffer
	checksum := sha256.Sum256([]byte("test"))

	if err := WriteTrailer(&buf, checksum, 100); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Magic(4) + SHA256(32) + Size(8) = 44 bytes
	expected := 4 + 32 + 8
	if buf.Len() != expected {
		t.Errorf("expected trailer size %d, got %d", expected, buf.Len())
	}
}

func TestResume_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	sessionID := "abc-123-def"
	agentName := "test-agent"
	storageName := "my-storage"

	if err := WriteResume(&buf, sessionID, agentName, storageName); err != nil {
		t.Fatalf("WriteResume: %v", err)
	}

	// ReadResume espera que o magic já foi lido pelo dispatcher
	// Lê e valida o magic manualmente
	var magic [4]byte
	if _, err := buf.Read(magic[:]); err != nil {
		t.Fatalf("reading magic: %v", err)
	}
	if magic != MagicResume {
		t.Fatalf("expected magic RSME, got %q", magic)
	}

	resume, err := ReadResume(&buf)
	if err != nil {
		t.Fatalf("ReadResume: %v", err)
	}

	if resume.SessionID != sessionID {
		t.Errorf("expected sessionID %q, got %q", sessionID, resume.SessionID)
	}
	if resume.AgentName != agentName {
		t.Errorf("expected agentName %q, got %q", agentName, resume.AgentName)
	}
	if resume.StorageName != storageName {
		t.Errorf("expected storageName %q, got %q", storageName, resume.StorageName)
	}
}

func TestSACK_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	offset := uint64(67108864) // 64MB

	if err := WriteSACK(&buf, offset); err != nil {
		t.Fatalf("WriteSACK: %v", err)
	}

	sack, err := ReadSACK(&buf)
	if err != nil {
		t.Fatalf("ReadSACK: %v", err)
	}

	if sack.Offset != offset {
		t.Errorf("expected offset %d, got %d", offset, sack.Offset)
	}
}

func TestResumeACK_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	status := ResumeStatusOK
	lastOffset := uint64(419430400) // 400MB

	if err := WriteResumeACK(&buf, status, lastOffset); err != nil {
		t.Fatalf("WriteResumeACK: %v", err)
	}

	rACK, err := ReadResumeACK(&buf)
	if err != nil {
		t.Fatalf("ReadResumeACK: %v", err)
	}

	if rACK.Status != status {
		t.Errorf("expected status %d, got %d", status, rACK.Status)
	}
	if rACK.LastOffset != lastOffset {
		t.Errorf("expected lastOffset %d, got %d", lastOffset, rACK.LastOffset)
	}
}

func TestParallelInit_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	maxStreams := uint8(4)
	chunkSize := uint32(262144) // 256KB

	if err := WriteParallelInit(&buf, maxStreams, chunkSize); err != nil {
		t.Fatalf("WriteParallelInit: %v", err)
	}

	pi, err := ReadParallelInit(&buf)
	if err != nil {
		t.Fatalf("ReadParallelInit: %v", err)
	}

	if pi.MaxStreams != maxStreams {
		t.Errorf("expected maxStreams %d, got %d", maxStreams, pi.MaxStreams)
	}
	if pi.ChunkSize != chunkSize {
		t.Errorf("expected chunkSize %d, got %d", chunkSize, pi.ChunkSize)
	}
}

func TestParallelInit_FrameSize(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteParallelInit(&buf, 4, 262144); err != nil {
		t.Fatalf("WriteParallelInit: %v", err)
	}
	// MaxStreams(1) + ChunkSize(4) = 5 bytes
	if buf.Len() != 5 {
		t.Errorf("expected ParallelInit size 5, got %d", buf.Len())
	}
}

func TestParallelJoin_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	sessionID := "abc-123-def-456"
	streamIndex := uint8(2)

	if err := WriteParallelJoin(&buf, sessionID, streamIndex); err != nil {
		t.Fatalf("WriteParallelJoin: %v", err)
	}

	// ReadParallelJoin espera que o magic já foi lido pelo dispatcher
	var magic [4]byte
	if _, err := buf.Read(magic[:]); err != nil {
		t.Fatalf("reading magic: %v", err)
	}
	if magic != MagicParallelJoin {
		t.Fatalf("expected magic PJIN, got %q", magic)
	}

	pj, err := ReadParallelJoin(&buf)
	if err != nil {
		t.Fatalf("ReadParallelJoin: %v", err)
	}

	if pj.SessionID != sessionID {
		t.Errorf("expected sessionID %q, got %q", sessionID, pj.SessionID)
	}
	if pj.StreamIndex != streamIndex {
		t.Errorf("expected streamIndex %d, got %d", streamIndex, pj.StreamIndex)
	}
}

func TestChunkSACK_RoundTrip(t *testing.T) {
	var buf bytes.Buffer

	streamIndex := uint8(3)
	chunkSeq := uint32(42)
	offset := uint64(134217728) // 128MB

	if err := WriteChunkSACK(&buf, streamIndex, chunkSeq, offset); err != nil {
		t.Fatalf("WriteChunkSACK: %v", err)
	}

	cs, err := ReadChunkSACK(&buf)
	if err != nil {
		t.Fatalf("ReadChunkSACK: %v", err)
	}

	if cs.StreamIndex != streamIndex {
		t.Errorf("expected streamIndex %d, got %d", streamIndex, cs.StreamIndex)
	}
	if cs.ChunkSeq != chunkSeq {
		t.Errorf("expected chunkSeq %d, got %d", chunkSeq, cs.ChunkSeq)
	}
	if cs.Offset != offset {
		t.Errorf("expected offset %d, got %d", offset, cs.Offset)
	}
}

func TestChunkSACK_FrameSize(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteChunkSACK(&buf, 1, 10, 1024); err != nil {
		t.Fatalf("WriteChunkSACK: %v", err)
	}
	// Magic(4) + StreamIndex(1) + ChunkSeq(4) + Offset(8) = 17 bytes
	if buf.Len() != 17 {
		t.Errorf("expected ChunkSACK size 17, got %d", buf.Len())
	}
}

func TestChunkSACK_InvalidMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("XXXX")) // magic errado
	buf.Write(make([]byte, 13))

	_, err := ReadChunkSACK(&buf)
	if err == nil {
		t.Fatal("expected error for invalid chunk sack magic")
	}
}

func TestParallelACK_RoundTrip(t *testing.T) {
	statuses := []byte{ParallelStatusOK, ParallelStatusFull, ParallelStatusNotFound}

	for _, status := range statuses {
		var buf bytes.Buffer

		if err := WriteParallelACK(&buf, status); err != nil {
			t.Fatalf("WriteParallelACK: %v", err)
		}

		got, err := ReadParallelACK(&buf)
		if err != nil {
			t.Fatalf("ReadParallelACK: %v", err)
		}

		if got != status {
			t.Errorf("expected status %d, got %d", status, got)
		}
	}
}
