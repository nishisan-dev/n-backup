// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// MagicControl é o magic byte enviado pelo agent ao conectar no canal de controle.
var MagicControl = [4]byte{'C', 'T', 'R', 'L'}

// MagicControlPing é o magic para frames ControlPing/ControlPong.
var MagicControlPing = [4]byte{'C', 'P', 'N', 'G'}

// MagicControlRotate é o magic para frames ControlRotate (Server → Agent).
var MagicControlRotate = [4]byte{'C', 'R', 'O', 'T'}

// MagicControlRotateACK é o magic para frames ControlRotateACK (Agent → Server).
var MagicControlRotateACK = [4]byte{'C', 'R', 'A', 'K'}

// MagicControlAdmit é o magic para frames ControlAdmit (Server → Agent).
var MagicControlAdmit = [4]byte{'C', 'A', 'D', 'M'}

// MagicControlDefer é o magic para frames ControlDefer (Server → Agent).
var MagicControlDefer = [4]byte{'C', 'D', 'F', 'E'}

// MagicControlAbort é o magic para frames ControlAbort (Server → Agent).
var MagicControlAbort = [4]byte{'C', 'A', 'B', 'T'}

// MagicControlProgress é o magic para frames ControlProgress (Agent → Server).
var MagicControlProgress = [4]byte{'C', 'P', 'R', 'G'}

// ControlPing é enviado pelo agent para o server no canal de controle.
// Formato: [Magic "CPNG" 4B] [Timestamp int64 8B]
type ControlPing struct {
	Timestamp int64 // UnixNano do momento do envio
}

// ControlPong é a resposta do server ao ControlPing.
// Formato: [Magic "CPNG" 4B] [Timestamp int64 8B] [ServerLoad float32 4B] [DiskFree uint32 4B]
type ControlPong struct {
	Timestamp  int64   // Echo do timestamp do ping (para cálculo de RTT)
	ServerLoad float32 // Carga do server (0.0 a 1.0)
	DiskFree   uint32  // Espaço livre em disco (MB)
}

// ControlRotate é enviado pelo server ao agent para solicitar rotação graceful de um stream.
// Formato: [Magic "CROT" 4B] [StreamIndex uint8 1B]
type ControlRotate struct {
	StreamIndex uint8 // Índice do stream a rotacionar
}

// ControlRotateACK é enviado pelo agent ao server confirmando que o stream foi drenado.
// Formato: [Magic "CRAK" 4B] [StreamIndex uint8 1B]
type ControlRotateACK struct {
	StreamIndex uint8 // Índice do stream drenado
}

// ControlAdmit é enviado pelo server ao agent para autorizar início de backup em um slot.
// Formato: [Magic "CADM" 4B] [SlotID uint8 1B]
type ControlAdmit struct {
	SlotID uint8
}

// ControlDefer é enviado pelo server ao agent pedindo que espere antes de iniciar backup.
// Formato: [Magic "CDFE" 4B] [WaitMinutes uint32 4B]
type ControlDefer struct {
	WaitMinutes uint32
}

// ControlAbort é enviado pelo server ao agent para abortar um backup em andamento.
// Formato: [Magic "CABT" 4B] [Reason uint32 4B]
type ControlAbort struct {
	Reason uint32
}

// Abort reasons.
const (
	AbortReasonDiskFull    uint32 = 1
	AbortReasonServerBusy  uint32 = 2
	AbortReasonMaintenance uint32 = 3
)

// ControlProgress é enviado pelo agent ao server para reportar progresso do backup.
// Formato: [Magic "CPRG" 4B] [TotalObjects uint32 4B] [ObjectsSent uint32 4B] [Flags uint8 1B]
// Flags: bit 0 = WalkComplete (1 = prescan finalizado, total confiável)
type ControlProgress struct {
	TotalObjects uint32
	ObjectsSent  uint32
	WalkComplete bool
}

// ReadControlMagic lê os 4 bytes de magic do canal de controle.
// Usado pelo dispatcher full-duplex para determinar o tipo de frame antes de parsear.
func ReadControlMagic(r io.Reader) ([4]byte, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return magic, fmt.Errorf("reading control magic: %w", err)
	}
	return magic, nil
}

// WriteControlPing escreve o frame ControlPing (Agent → Server).
func WriteControlPing(w io.Writer, timestamp int64) error {
	buf := make([]byte, 12) // 4B magic + 8B timestamp
	copy(buf[0:4], MagicControlPing[:])
	binary.BigEndian.PutUint64(buf[4:12], uint64(timestamp))
	_, err := w.Write(buf)
	return err
}

// ReadControlPingPayload lê o payload de ControlPing (8B timestamp) após o magic já ter sido lido.
func ReadControlPingPayload(r io.Reader) (int64, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control ping payload: %w", err)
	}
	return int64(binary.BigEndian.Uint64(buf)), nil
}

// ReadControlPing lê o frame ControlPing completo (magic + payload).
func ReadControlPing(r io.Reader) (int64, error) {
	buf := make([]byte, 12)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control ping: %w", err)
	}
	if buf[0] != MagicControlPing[0] || buf[1] != MagicControlPing[1] ||
		buf[2] != MagicControlPing[2] || buf[3] != MagicControlPing[3] {
		return 0, fmt.Errorf("%w: expected CPNG, got %q", ErrInvalidMagic, string(buf[0:4]))
	}
	timestamp := int64(binary.BigEndian.Uint64(buf[4:12]))
	return timestamp, nil
}

// WriteControlPong escreve o frame ControlPong (Server → Agent).
func WriteControlPong(w io.Writer, timestamp int64, serverLoad float32, diskFree uint32) error {
	buf := make([]byte, 20) // 4B magic + 8B timestamp + 4B load + 4B disk
	copy(buf[0:4], MagicControlPing[:])
	binary.BigEndian.PutUint64(buf[4:12], uint64(timestamp))
	binary.BigEndian.PutUint32(buf[12:16], math.Float32bits(serverLoad))
	binary.BigEndian.PutUint32(buf[16:20], diskFree)
	_, err := w.Write(buf)
	return err
}

// ReadControlPongPayload lê o payload de ControlPong (16B) após o magic já ter sido lido.
func ReadControlPongPayload(r io.Reader) (*ControlPong, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("reading control pong payload: %w", err)
	}
	return &ControlPong{
		Timestamp:  int64(binary.BigEndian.Uint64(buf[0:8])),
		ServerLoad: math.Float32frombits(binary.BigEndian.Uint32(buf[8:12])),
		DiskFree:   binary.BigEndian.Uint32(buf[12:16]),
	}, nil
}

// ReadControlPong lê o frame ControlPong completo (magic + payload).
func ReadControlPong(r io.Reader) (*ControlPong, error) {
	buf := make([]byte, 20)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("reading control pong: %w", err)
	}
	if buf[0] != MagicControlPing[0] || buf[1] != MagicControlPing[1] ||
		buf[2] != MagicControlPing[2] || buf[3] != MagicControlPing[3] {
		return nil, fmt.Errorf("%w: expected CPNG, got %q", ErrInvalidMagic, string(buf[0:4]))
	}
	return &ControlPong{
		Timestamp:  int64(binary.BigEndian.Uint64(buf[4:12])),
		ServerLoad: math.Float32frombits(binary.BigEndian.Uint32(buf[12:16])),
		DiskFree:   binary.BigEndian.Uint32(buf[16:20]),
	}, nil
}

// WriteControlRotate escreve o frame ControlRotate (Server → Agent).
func WriteControlRotate(w io.Writer, streamIndex uint8) error {
	buf := make([]byte, 5) // 4B magic + 1B stream index
	copy(buf[0:4], MagicControlRotate[:])
	buf[4] = streamIndex
	_, err := w.Write(buf)
	return err
}

// ReadControlRotatePayload lê o payload de ControlRotate (1B) após o magic já ter sido lido.
func ReadControlRotatePayload(r io.Reader) (uint8, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control rotate payload: %w", err)
	}
	return buf[0], nil
}

// ReadControlRotate lê o frame ControlRotate completo (magic + payload).
func ReadControlRotate(r io.Reader) (uint8, error) {
	buf := make([]byte, 5)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control rotate: %w", err)
	}
	if buf[0] != MagicControlRotate[0] || buf[1] != MagicControlRotate[1] ||
		buf[2] != MagicControlRotate[2] || buf[3] != MagicControlRotate[3] {
		return 0, fmt.Errorf("%w: expected CROT, got %q", ErrInvalidMagic, string(buf[0:4]))
	}
	return buf[4], nil
}

// WriteControlRotateACK escreve o frame ControlRotateACK (Agent → Server).
func WriteControlRotateACK(w io.Writer, streamIndex uint8) error {
	buf := make([]byte, 5) // 4B magic + 1B stream index
	copy(buf[0:4], MagicControlRotateACK[:])
	buf[4] = streamIndex
	_, err := w.Write(buf)
	return err
}

// ReadControlRotateACKPayload lê o payload de ControlRotateACK (1B) após o magic já ter sido lido.
func ReadControlRotateACKPayload(r io.Reader) (uint8, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control rotate ack payload: %w", err)
	}
	return buf[0], nil
}

// ReadControlRotateACK lê o frame ControlRotateACK completo (magic + payload).
func ReadControlRotateACK(r io.Reader) (uint8, error) {
	buf := make([]byte, 5)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control rotate ack: %w", err)
	}
	if buf[0] != MagicControlRotateACK[0] || buf[1] != MagicControlRotateACK[1] ||
		buf[2] != MagicControlRotateACK[2] || buf[3] != MagicControlRotateACK[3] {
		return 0, fmt.Errorf("%w: expected CRAK, got %q", ErrInvalidMagic, string(buf[0:4]))
	}
	return buf[4], nil
}

// WriteControlAdmit escreve o frame ControlAdmit (Server → Agent).
func WriteControlAdmit(w io.Writer, slotID uint8) error {
	buf := make([]byte, 5) // 4B magic + 1B slot
	copy(buf[0:4], MagicControlAdmit[:])
	buf[4] = slotID
	_, err := w.Write(buf)
	return err
}

// ReadControlAdmitPayload lê o payload de ControlAdmit (1B) após o magic já ter sido lido.
func ReadControlAdmitPayload(r io.Reader) (uint8, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control admit payload: %w", err)
	}
	return buf[0], nil
}

// WriteControlDefer escreve o frame ControlDefer (Server → Agent).
func WriteControlDefer(w io.Writer, waitMinutes uint32) error {
	buf := make([]byte, 8) // 4B magic + 4B wait
	copy(buf[0:4], MagicControlDefer[:])
	binary.BigEndian.PutUint32(buf[4:8], waitMinutes)
	_, err := w.Write(buf)
	return err
}

// ReadControlDeferPayload lê o payload de ControlDefer (4B) após o magic já ter sido lido.
func ReadControlDeferPayload(r io.Reader) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control defer payload: %w", err)
	}
	return binary.BigEndian.Uint32(buf), nil
}

// WriteControlAbort escreve o frame ControlAbort (Server → Agent).
func WriteControlAbort(w io.Writer, reason uint32) error {
	buf := make([]byte, 8) // 4B magic + 4B reason
	copy(buf[0:4], MagicControlAbort[:])
	binary.BigEndian.PutUint32(buf[4:8], reason)
	_, err := w.Write(buf)
	return err
}

// ReadControlAbortPayload lê o payload de ControlAbort (4B) após o magic já ter sido lido.
func ReadControlAbortPayload(r io.Reader) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("reading control abort payload: %w", err)
	}
	return binary.BigEndian.Uint32(buf), nil
}

// WriteControlProgress escreve o frame ControlProgress (Agent → Server).
func WriteControlProgress(w io.Writer, totalObjects, objectsSent uint32, walkComplete bool) error {
	buf := make([]byte, 13) // 4B magic + 4B total + 4B sent + 1B flags
	copy(buf[0:4], MagicControlProgress[:])
	binary.BigEndian.PutUint32(buf[4:8], totalObjects)
	binary.BigEndian.PutUint32(buf[8:12], objectsSent)
	if walkComplete {
		buf[12] = 1
	}
	_, err := w.Write(buf)
	return err
}

// ReadControlProgressPayload lê o payload de ControlProgress (9B) após o magic já ter sido lido.
func ReadControlProgressPayload(r io.Reader) (*ControlProgress, error) {
	buf := make([]byte, 9)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("reading control progress payload: %w", err)
	}
	return &ControlProgress{
		TotalObjects: binary.BigEndian.Uint32(buf[0:4]),
		ObjectsSent:  binary.BigEndian.Uint32(buf[4:8]),
		WalkComplete: buf[8]&1 != 0,
	}, nil
}

// ReadControlProgress lê o frame ControlProgress completo (magic + payload).
func ReadControlProgress(r io.Reader) (*ControlProgress, error) {
	buf := make([]byte, 13)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("reading control progress: %w", err)
	}
	if buf[0] != MagicControlProgress[0] || buf[1] != MagicControlProgress[1] ||
		buf[2] != MagicControlProgress[2] || buf[3] != MagicControlProgress[3] {
		return nil, fmt.Errorf("%w: expected CPRG, got %q", ErrInvalidMagic, string(buf[0:4]))
	}
	return &ControlProgress{
		TotalObjects: binary.BigEndian.Uint32(buf[4:8]),
		ObjectsSent:  binary.BigEndian.Uint32(buf[8:12]),
		WalkComplete: buf[12]&1 != 0,
	}, nil
}
