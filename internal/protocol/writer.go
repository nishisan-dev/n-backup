// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WriteHandshake escreve o frame de handshake (Client → Server).
// Formato: [Magic 4B] [Version 1B] [AgentName UTF-8] ['\n' 1B] [StorageName UTF-8] ['\n' 1B]
func WriteHandshake(w io.Writer, agentName, storageName string) error {
	if _, err := w.Write(MagicHandshake[:]); err != nil {
		return fmt.Errorf("writing handshake magic: %w", err)
	}
	if _, err := w.Write([]byte{ProtocolVersion}); err != nil {
		return fmt.Errorf("writing handshake version: %w", err)
	}
	if _, err := w.Write([]byte(agentName)); err != nil {
		return fmt.Errorf("writing agent name: %w", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing agent name delimiter: %w", err)
	}
	if _, err := w.Write([]byte(storageName)); err != nil {
		return fmt.Errorf("writing storage name: %w", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing storage name delimiter: %w", err)
	}
	return nil
}

// WriteACK escreve o frame ACK (Server → Client).
// Formato: [Status 1B] [Message UTF-8 (opt)] ['\n' 1B] [SessionID UTF-8 (opt)] ['\n' 1B]
func WriteACK(w io.Writer, status byte, message string, sessionID string) error {
	if _, err := w.Write([]byte{status}); err != nil {
		return fmt.Errorf("writing ack status: %w", err)
	}
	if message != "" {
		if _, err := w.Write([]byte(message)); err != nil {
			return fmt.Errorf("writing ack message: %w", err)
		}
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing ack delimiter: %w", err)
	}
	if _, err := w.Write([]byte(sessionID)); err != nil {
		return fmt.Errorf("writing ack session id: %w", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing ack session delimiter: %w", err)
	}
	return nil
}

// WriteTrailer escreve o frame trailer (Client → Server).
// Formato: [Magic "DONE" 4B] [SHA-256 32B] [Size uint64 8B]
func WriteTrailer(w io.Writer, checksum [32]byte, size uint64) error {
	if _, err := w.Write(MagicTrailer[:]); err != nil {
		return fmt.Errorf("writing trailer magic: %w", err)
	}
	if _, err := w.Write(checksum[:]); err != nil {
		return fmt.Errorf("writing trailer checksum: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, size); err != nil {
		return fmt.Errorf("writing trailer size: %w", err)
	}
	return nil
}

// WriteFinalACK escreve o frame Final ACK (Server → Client).
// Formato: [Status 1B]
func WriteFinalACK(w io.Writer, status byte) error {
	if _, err := w.Write([]byte{status}); err != nil {
		return fmt.Errorf("writing final ack: %w", err)
	}
	return nil
}

// WritePing escreve o frame PING (Client → Server para health check).
// Formato: [Magic "PING" 4B]
func WritePing(w io.Writer) error {
	if _, err := w.Write(MagicPing[:]); err != nil {
		return fmt.Errorf("writing ping: %w", err)
	}
	return nil
}

// WriteHealthResponse escreve a resposta do health check (Server → Client).
// Formato: [Status 1B] [DiskFree uint64 8B] ['\n' 1B]
func WriteHealthResponse(w io.Writer, status byte, diskFree uint64) error {
	if _, err := w.Write([]byte{status}); err != nil {
		return fmt.Errorf("writing health status: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, diskFree); err != nil {
		return fmt.Errorf("writing health disk free: %w", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing health delimiter: %w", err)
	}
	return nil
}

// WriteResume escreve o frame RESUME (Client → Server).
// Formato: [Magic "RSME" 4B] [Version 1B] [SessionID UTF-8] ['\n' 1B] [AgentName UTF-8] ['\n' 1B] [StorageName UTF-8] ['\n' 1B]
func WriteResume(w io.Writer, sessionID, agentName, storageName string) error {
	if _, err := w.Write(MagicResume[:]); err != nil {
		return fmt.Errorf("writing resume magic: %w", err)
	}
	if _, err := w.Write([]byte{ProtocolVersion}); err != nil {
		return fmt.Errorf("writing resume version: %w", err)
	}
	for _, field := range []string{sessionID, agentName, storageName} {
		if _, err := w.Write([]byte(field)); err != nil {
			return fmt.Errorf("writing resume field: %w", err)
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("writing resume delimiter: %w", err)
		}
	}
	return nil
}

// WriteSACK escreve o frame SACK (Server → Client).
// Formato: [Magic "SACK" 4B] [Offset uint64 8B]
func WriteSACK(w io.Writer, offset uint64) error {
	if _, err := w.Write(MagicSACK[:]); err != nil {
		return fmt.Errorf("writing sack magic: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, offset); err != nil {
		return fmt.Errorf("writing sack offset: %w", err)
	}
	return nil
}

// WriteResumeACK escreve o frame Resume ACK (Server → Client).
// Formato: [Status 1B] [LastOffset uint64 8B]
func WriteResumeACK(w io.Writer, status byte, lastOffset uint64) error {
	if _, err := w.Write([]byte{status}); err != nil {
		return fmt.Errorf("writing resume ack status: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, lastOffset); err != nil {
		return fmt.Errorf("writing resume ack offset: %w", err)
	}
	return nil
}
