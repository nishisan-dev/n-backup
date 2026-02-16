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

// WriteControlPing escreve o frame ControlPing (Agent → Server).
func WriteControlPing(w io.Writer, timestamp int64) error {
	buf := make([]byte, 12) // 4B magic + 8B timestamp
	copy(buf[0:4], MagicControlPing[:])
	binary.BigEndian.PutUint64(buf[4:12], uint64(timestamp))
	_, err := w.Write(buf)
	return err
}

// ReadControlPing lê o frame ControlPing (Agent → Server).
// O magic "CPNG" é lido e validado.
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

// ReadControlPong lê o frame ControlPong (Server → Agent).
// O magic "CPNG" é lido e validado.
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
