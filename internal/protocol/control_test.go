// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"bytes"
	"testing"
)

func TestControlPing_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	ts := int64(1739700000000000000) // UnixNano fixo

	if err := WriteControlPing(&buf, ts); err != nil {
		t.Fatalf("WriteControlPing failed: %v", err)
	}

	// Verifica tamanho do frame: 4B magic + 8B timestamp = 12B
	if buf.Len() != 12 {
		t.Fatalf("expected 12 bytes, got %d", buf.Len())
	}

	got, err := ReadControlPing(&buf)
	if err != nil {
		t.Fatalf("ReadControlPing failed: %v", err)
	}

	if got != ts {
		t.Errorf("timestamp mismatch: want %d, got %d", ts, got)
	}
}

func TestControlPong_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	ts := int64(1739700000000000000)
	load := float32(0.42)
	disk := uint32(102400)

	if err := WriteControlPong(&buf, ts, load, disk); err != nil {
		t.Fatalf("WriteControlPong failed: %v", err)
	}

	// Verifica tamanho do frame: 4B magic + 8B timestamp + 4B load + 4B disk = 20B
	if buf.Len() != 20 {
		t.Fatalf("expected 20 bytes, got %d", buf.Len())
	}

	got, err := ReadControlPong(&buf)
	if err != nil {
		t.Fatalf("ReadControlPong failed: %v", err)
	}

	if got.Timestamp != ts {
		t.Errorf("timestamp: want %d, got %d", ts, got.Timestamp)
	}
	if got.ServerLoad != load {
		t.Errorf("server_load: want %f, got %f", load, got.ServerLoad)
	}
	if got.DiskFree != disk {
		t.Errorf("disk_free: want %d, got %d", disk, got.DiskFree)
	}
}

func TestControlPing_InvalidMagic(t *testing.T) {
	buf := bytes.NewBufferString("BAD!12345678")
	_, err := ReadControlPing(buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestControlPong_InvalidMagic(t *testing.T) {
	buf := bytes.NewBufferString("BAD!1234567890123456")
	_, err := ReadControlPong(buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestControlPong_ZeroValues(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteControlPong(&buf, 0, 0.0, 0); err != nil {
		t.Fatalf("WriteControlPong failed: %v", err)
	}

	got, err := ReadControlPong(&buf)
	if err != nil {
		t.Fatalf("ReadControlPong failed: %v", err)
	}

	if got.Timestamp != 0 || got.ServerLoad != 0.0 || got.DiskFree != 0 {
		t.Errorf("expected zero values, got %+v", got)
	}
}

func TestControlPong_MaxLoad(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteControlPong(&buf, 0, 1.0, 4294967295); err != nil {
		t.Fatalf("WriteControlPong failed: %v", err)
	}

	got, err := ReadControlPong(&buf)
	if err != nil {
		t.Fatalf("ReadControlPong failed: %v", err)
	}

	if got.ServerLoad != 1.0 {
		t.Errorf("server_load: want 1.0, got %f", got.ServerLoad)
	}
	if got.DiskFree != 4294967295 {
		t.Errorf("disk_free: want max uint32, got %d", got.DiskFree)
	}
}
