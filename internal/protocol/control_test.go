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

func TestReadControlMagic(t *testing.T) {
	tests := []struct {
		name  string
		magic [4]byte
	}{
		{"CPNG", MagicControlPing},
		{"CROT", MagicControlRotate},
		{"CRAK", MagicControlRotateACK},
		{"CADM", MagicControlAdmit},
		{"CDFE", MagicControlDefer},
		{"CABT", MagicControlAbort},
		{"CPRG", MagicControlProgress},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBuffer(tt.magic[:])
			got, err := ReadControlMagic(buf)
			if err != nil {
				t.Fatalf("ReadControlMagic failed: %v", err)
			}
			if got != tt.magic {
				t.Errorf("magic mismatch: want %q, got %q", tt.magic, got)
			}
		})
	}
}

func TestControlRotate_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	streamIdx := uint8(3)

	if err := WriteControlRotate(&buf, streamIdx); err != nil {
		t.Fatalf("WriteControlRotate failed: %v", err)
	}

	if buf.Len() != 5 {
		t.Fatalf("expected 5 bytes, got %d", buf.Len())
	}

	got, err := ReadControlRotate(&buf)
	if err != nil {
		t.Fatalf("ReadControlRotate failed: %v", err)
	}

	if got != streamIdx {
		t.Errorf("stream index: want %d, got %d", streamIdx, got)
	}
}

func TestControlRotate_InvalidMagic(t *testing.T) {
	buf := bytes.NewBufferString("BAD!X")
	_, err := ReadControlRotate(buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestControlRotateACK_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	streamIdx := uint8(7)

	if err := WriteControlRotateACK(&buf, streamIdx); err != nil {
		t.Fatalf("WriteControlRotateACK failed: %v", err)
	}

	if buf.Len() != 5 {
		t.Fatalf("expected 5 bytes, got %d", buf.Len())
	}

	got, err := ReadControlRotateACK(&buf)
	if err != nil {
		t.Fatalf("ReadControlRotateACK failed: %v", err)
	}

	if got != streamIdx {
		t.Errorf("stream index: want %d, got %d", streamIdx, got)
	}
}

func TestControlRotateACK_InvalidMagic(t *testing.T) {
	buf := bytes.NewBufferString("BAD!X")
	_, err := ReadControlRotateACK(buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestControlRotate_PayloadAfterMagic(t *testing.T) {
	var buf bytes.Buffer
	streamIdx := uint8(5)
	WriteControlRotate(&buf, streamIdx)

	// Lê magic separado
	magic, err := ReadControlMagic(&buf)
	if err != nil {
		t.Fatalf("ReadControlMagic failed: %v", err)
	}
	if magic != MagicControlRotate {
		t.Fatalf("expected CROT magic, got %q", magic)
	}

	// Lê payload
	got, err := ReadControlRotatePayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlRotatePayload failed: %v", err)
	}
	if got != streamIdx {
		t.Errorf("stream index: want %d, got %d", streamIdx, got)
	}
}

func TestControlRotateACK_PayloadAfterMagic(t *testing.T) {
	var buf bytes.Buffer
	streamIdx := uint8(2)
	WriteControlRotateACK(&buf, streamIdx)

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlRotateACK {
		t.Fatalf("expected CRAK magic, got %q", magic)
	}

	got, err := ReadControlRotateACKPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlRotateACKPayload failed: %v", err)
	}
	if got != streamIdx {
		t.Errorf("stream index: want %d, got %d", streamIdx, got)
	}
}

func TestControlAdmit_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	slotID := uint8(4)

	if err := WriteControlAdmit(&buf, slotID); err != nil {
		t.Fatalf("WriteControlAdmit failed: %v", err)
	}
	if buf.Len() != 5 {
		t.Fatalf("expected 5 bytes, got %d", buf.Len())
	}

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlAdmit {
		t.Fatalf("expected CADM magic, got %q", magic)
	}
	got, err := ReadControlAdmitPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlAdmitPayload failed: %v", err)
	}
	if got != slotID {
		t.Errorf("slot ID: want %d, got %d", slotID, got)
	}
}

func TestControlDefer_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	waitMin := uint32(15)

	if err := WriteControlDefer(&buf, waitMin); err != nil {
		t.Fatalf("WriteControlDefer failed: %v", err)
	}
	if buf.Len() != 8 {
		t.Fatalf("expected 8 bytes, got %d", buf.Len())
	}

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlDefer {
		t.Fatalf("expected CDFE magic, got %q", magic)
	}
	got, err := ReadControlDeferPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlDeferPayload failed: %v", err)
	}
	if got != waitMin {
		t.Errorf("wait minutes: want %d, got %d", waitMin, got)
	}
}

func TestControlAbort_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	reason := AbortReasonDiskFull

	if err := WriteControlAbort(&buf, reason); err != nil {
		t.Fatalf("WriteControlAbort failed: %v", err)
	}
	if buf.Len() != 8 {
		t.Fatalf("expected 8 bytes, got %d", buf.Len())
	}

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlAbort {
		t.Fatalf("expected CABT magic, got %q", magic)
	}
	got, err := ReadControlAbortPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlAbortPayload failed: %v", err)
	}
	if got != reason {
		t.Errorf("reason: want %d, got %d", reason, got)
	}
}

func TestControlPong_PayloadAfterMagic(t *testing.T) {
	var buf bytes.Buffer
	ts := int64(1739700000000000000)
	load := float32(0.75)
	disk := uint32(50000)

	WriteControlPong(&buf, ts, load, disk)

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlPing {
		t.Fatalf("expected CPNG magic, got %q", magic)
	}

	got, err := ReadControlPongPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlPongPayload failed: %v", err)
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

func TestControlPing_PayloadAfterMagic(t *testing.T) {
	var buf bytes.Buffer
	ts := int64(1739700000000000000)

	WriteControlPing(&buf, ts)

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlPing {
		t.Fatalf("expected CPNG magic, got %q", magic)
	}

	got, err := ReadControlPingPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlPingPayload failed: %v", err)
	}
	if got != ts {
		t.Errorf("timestamp: want %d, got %d", ts, got)
	}
}

func TestControlProgress_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	total := uint32(15000)
	sent := uint32(3200)

	if err := WriteControlProgress(&buf, total, sent, true); err != nil {
		t.Fatalf("WriteControlProgress failed: %v", err)
	}
	if buf.Len() != 13 {
		t.Fatalf("expected 13 bytes, got %d", buf.Len())
	}

	got, err := ReadControlProgress(&buf)
	if err != nil {
		t.Fatalf("ReadControlProgress failed: %v", err)
	}
	if got.TotalObjects != total {
		t.Errorf("total_objects: want %d, got %d", total, got.TotalObjects)
	}
	if got.ObjectsSent != sent {
		t.Errorf("objects_sent: want %d, got %d", sent, got.ObjectsSent)
	}
	if !got.WalkComplete {
		t.Error("walk_complete: want true, got false")
	}
}

func TestControlProgress_WalkIncomplete(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteControlProgress(&buf, 500, 100, false); err != nil {
		t.Fatalf("WriteControlProgress failed: %v", err)
	}

	got, err := ReadControlProgress(&buf)
	if err != nil {
		t.Fatalf("ReadControlProgress failed: %v", err)
	}
	if got.WalkComplete {
		t.Error("walk_complete: want false, got true")
	}
}

func TestControlProgress_InvalidMagic(t *testing.T) {
	buf := bytes.NewBufferString("BAD!123456789")
	_, err := ReadControlProgress(buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestControlProgress_PayloadAfterMagic(t *testing.T) {
	var buf bytes.Buffer
	total := uint32(8000)
	sent := uint32(4500)
	WriteControlProgress(&buf, total, sent, true)

	magic, _ := ReadControlMagic(&buf)
	if magic != MagicControlProgress {
		t.Fatalf("expected CPRG magic, got %q", magic)
	}

	got, err := ReadControlProgressPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlProgressPayload failed: %v", err)
	}
	if got.TotalObjects != total {
		t.Errorf("total: want %d, got %d", total, got.TotalObjects)
	}
	if got.ObjectsSent != sent {
		t.Errorf("sent: want %d, got %d", sent, got.ObjectsSent)
	}
	if !got.WalkComplete {
		t.Error("walk_complete: want true, got false")
	}
}

func TestControlStatsPayload_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	cpu := float32(45.5)
	mem := float32(72.3)
	disk := float32(88.1)
	load := float32(2.15)

	if err := WriteControlStatsPayload(&buf, cpu, mem, disk, load); err != nil {
		t.Fatalf("WriteControlStatsPayload failed: %v", err)
	}

	// Payload sem magic: 4 × float32 = 16B
	if buf.Len() != 16 {
		t.Fatalf("expected 16 bytes, got %d", buf.Len())
	}

	got, err := ReadControlStatsPayload(&buf)
	if err != nil {
		t.Fatalf("ReadControlStatsPayload failed: %v", err)
	}

	if got.CPUPercent != cpu {
		t.Errorf("cpu: want %f, got %f", cpu, got.CPUPercent)
	}
	if got.MemoryPercent != mem {
		t.Errorf("mem: want %f, got %f", mem, got.MemoryPercent)
	}
	if got.DiskUsagePercent != disk {
		t.Errorf("disk: want %f, got %f", disk, got.DiskUsagePercent)
	}
	if got.LoadAverage != load {
		t.Errorf("load: want %f, got %f", load, got.LoadAverage)
	}
}
