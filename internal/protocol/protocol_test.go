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

	if err := WriteHandshake(&buf, agentName, storageName); err != nil {
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
}

func TestACK_RoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		status  byte
		message string
	}{
		{"GO with empty message", StatusGo, ""},
		{"FULL with message", StatusFull, "disk is full"},
		{"BUSY with message", StatusBusy, "backup in progress"},
		{"REJECT with message", StatusReject, "agent not authorized"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			if err := WriteACK(&buf, tt.status, tt.message); err != nil {
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

	if err := WriteHandshake(&buf, agentName, storageName); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	// Magic(4) + Version(1) + AgentName(14) + Delimiter(1) + StorageName(7) + Delimiter(1) = 28 bytes
	expected := 4 + 1 + len(agentName) + 1 + len(storageName) + 1
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
