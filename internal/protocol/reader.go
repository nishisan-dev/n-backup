// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// ReadHandshake lê e valida o frame de handshake (Client → Server).
func ReadHandshake(r io.Reader) (*Handshake, error) {
	// Lê magic
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("reading handshake magic: %w", err)
	}
	if magic != MagicHandshake {
		return nil, ErrInvalidMagic
	}

	// Lê version
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return nil, fmt.Errorf("reading handshake version: %w", err)
	}
	if version[0] != ProtocolVersion {
		return nil, ErrInvalidVersion
	}

	// Lê agent name até '\n'
	br := bufio.NewReader(r)
	name, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading agent name: %w", err)
	}
	name = name[:len(name)-1]

	// Lê storage name até '\n'
	storageName, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading storage name: %w", err)
	}
	storageName = storageName[:len(storageName)-1]

	return &Handshake{
		Version:     version[0],
		AgentName:   name,
		StorageName: storageName,
	}, nil
}

// ReadACK lê o frame ACK (Server → Client).
func ReadACK(r io.Reader) (*ACK, error) {
	// Lê status
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, fmt.Errorf("reading ack status: %w", err)
	}

	// Lê message até '\n'
	br := bufio.NewReader(r)
	msg, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading ack message: %w", err)
	}
	msg = msg[:len(msg)-1]

	// Lê sessionID até '\n'
	sessionID, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading ack session id: %w", err)
	}
	sessionID = sessionID[:len(sessionID)-1]

	return &ACK{
		Status:    status[0],
		Message:   msg,
		SessionID: sessionID,
	}, nil
}

// ReadTrailer lê o frame trailer (Client → Server).
func ReadTrailer(r io.Reader) (*Trailer, error) {
	// Lê magic
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("reading trailer magic: %w", err)
	}
	if magic != MagicTrailer {
		return nil, ErrInvalidMagic
	}

	// Lê checksum SHA-256
	var checksum [32]byte
	if _, err := io.ReadFull(r, checksum[:]); err != nil {
		return nil, fmt.Errorf("reading trailer checksum: %w", err)
	}

	// Lê size
	var size uint64
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return nil, fmt.Errorf("reading trailer size: %w", err)
	}

	return &Trailer{
		Checksum: checksum,
		Size:     size,
	}, nil
}

// ReadFinalACK lê o frame Final ACK (Server → Client).
func ReadFinalACK(r io.Reader) (*FinalACK, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, fmt.Errorf("reading final ack: %w", err)
	}
	return &FinalACK{Status: status[0]}, nil
}

// ReadPing lê e valida o frame PING (Client → Server).
func ReadPing(r io.Reader) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("reading ping magic: %w", err)
	}
	if magic != MagicPing {
		return ErrInvalidMagic
	}
	return nil
}

// ReadHealthResponse lê a resposta do health check (Server → Client).
func ReadHealthResponse(r io.Reader) (*HealthResponse, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, fmt.Errorf("reading health status: %w", err)
	}

	var diskFree uint64
	if err := binary.Read(r, binary.BigEndian, &diskFree); err != nil {
		return nil, fmt.Errorf("reading health disk free: %w", err)
	}

	// Lê delimiter '\n'
	var delim [1]byte
	if _, err := io.ReadFull(r, delim[:]); err != nil {
		return nil, fmt.Errorf("reading health delimiter: %w", err)
	}

	return &HealthResponse{
		Status:   status[0],
		DiskFree: diskFree,
	}, nil
}

// ReadResume lê o frame RESUME (Client → Server).
// O magic "RSME" já foi lido pelo dispatcher; lê version + sessionID + agentName + storageName.
func ReadResume(r io.Reader) (*Resume, error) {
	// Lê version
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return nil, fmt.Errorf("reading resume version: %w", err)
	}
	if version[0] != ProtocolVersion {
		return nil, ErrInvalidVersion
	}

	br := bufio.NewReader(r)

	sessionID, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading resume session id: %w", err)
	}
	sessionID = sessionID[:len(sessionID)-1]

	agentName, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading resume agent name: %w", err)
	}
	agentName = agentName[:len(agentName)-1]

	storageName, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading resume storage name: %w", err)
	}
	storageName = storageName[:len(storageName)-1]

	return &Resume{
		SessionID:   sessionID,
		AgentName:   agentName,
		StorageName: storageName,
	}, nil
}

// ReadSACK lê o frame SACK (Server → Client).
func ReadSACK(r io.Reader) (*SACK, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("reading sack magic: %w", err)
	}
	if magic != MagicSACK {
		return nil, ErrInvalidMagic
	}

	var offset uint64
	if err := binary.Read(r, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("reading sack offset: %w", err)
	}

	return &SACK{Offset: offset}, nil
}

// ReadResumeACK lê o frame Resume ACK (Server → Client).
func ReadResumeACK(r io.Reader) (*ResumeACK, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, fmt.Errorf("reading resume ack status: %w", err)
	}

	var lastOffset uint64
	if err := binary.Read(r, binary.BigEndian, &lastOffset); err != nil {
		return nil, fmt.Errorf("reading resume ack offset: %w", err)
	}

	return &ResumeACK{
		Status:     status[0],
		LastOffset: lastOffset,
	}, nil
}

// ReadParallelInit lê a extensão ParallelInit do handshake (Client → Server).
// Formato: [MaxStreams uint8 1B] [ChunkSize uint32 4B]
func ReadParallelInit(r io.Reader) (*ParallelInit, error) {
	var maxStreams [1]byte
	if _, err := io.ReadFull(r, maxStreams[:]); err != nil {
		return nil, fmt.Errorf("reading parallel init max streams: %w", err)
	}

	var chunkSize uint32
	if err := binary.Read(r, binary.BigEndian, &chunkSize); err != nil {
		return nil, fmt.Errorf("reading parallel init chunk size: %w", err)
	}

	return &ParallelInit{
		MaxStreams: maxStreams[0],
		ChunkSize:  chunkSize,
	}, nil
}

// ReadParallelInitAfterMaxStreams lê o restante do ParallelInit quando o byte
// MaxStreams já foi consumido pelo discriminador de modo (handler.go).
// Lê apenas ChunkSize (4B) e reconstrói o ParallelInit completo.
func ReadParallelInitAfterMaxStreams(r io.Reader, maxStreams uint8) (*ParallelInit, error) {
	var chunkSize uint32
	if err := binary.Read(r, binary.BigEndian, &chunkSize); err != nil {
		return nil, fmt.Errorf("reading parallel init chunk size: %w", err)
	}

	return &ParallelInit{
		MaxStreams: maxStreams,
		ChunkSize:  chunkSize,
	}, nil
}

// ReadParallelJoin lê o frame ParallelJoin (Client → Server).
// O magic "PJIN" já foi lido pelo dispatcher; lê version + sessionID + streamIndex.
func ReadParallelJoin(r io.Reader) (*ParallelJoin, error) {
	// Lê version
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return nil, fmt.Errorf("reading parallel join version: %w", err)
	}
	if version[0] != ProtocolVersion {
		return nil, ErrInvalidVersion
	}

	// Lê sessionID até '\n'
	br := bufio.NewReader(r)
	sessionID, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading parallel join session id: %w", err)
	}
	sessionID = sessionID[:len(sessionID)-1]

	// Lê streamIndex
	var streamIndex [1]byte
	if _, err := io.ReadFull(br, streamIndex[:]); err != nil {
		return nil, fmt.Errorf("reading parallel join stream index: %w", err)
	}

	return &ParallelJoin{
		SessionID:   sessionID,
		StreamIndex: streamIndex[0],
	}, nil
}

// ReadParallelACK lê a resposta ao ParallelJoin (Server → Client).
// Formato: [Status 1B]
func ReadParallelACK(r io.Reader) (byte, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return 0, fmt.Errorf("reading parallel ack: %w", err)
	}
	return status[0], nil
}

// ReadChunkSACK lê o frame ChunkSACK (Server → Client).
func ReadChunkSACK(r io.Reader) (*ChunkSACK, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("reading chunk sack magic: %w", err)
	}
	if magic != MagicChunkSACK {
		return nil, ErrInvalidMagic
	}

	var streamIndex [1]byte
	if _, err := io.ReadFull(r, streamIndex[:]); err != nil {
		return nil, fmt.Errorf("reading chunk sack stream index: %w", err)
	}

	var chunkSeq uint32
	if err := binary.Read(r, binary.BigEndian, &chunkSeq); err != nil {
		return nil, fmt.Errorf("reading chunk sack chunk seq: %w", err)
	}

	var offset uint64
	if err := binary.Read(r, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("reading chunk sack offset: %w", err)
	}

	return &ChunkSACK{
		StreamIndex: streamIndex[0],
		ChunkSeq:    chunkSeq,
		Offset:      offset,
	}, nil
}

// ReadChunkHeader lê o header de chunk paralelo (Client → Server).
// Formato: [GlobalSeq uint32 4B] [Length uint32 4B]
func ReadChunkHeader(r io.Reader) (*ChunkHeader, error) {
	var globalSeq uint32
	if err := binary.Read(r, binary.BigEndian, &globalSeq); err != nil {
		return nil, fmt.Errorf("reading chunk header seq: %w", err)
	}
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("reading chunk header length: %w", err)
	}
	return &ChunkHeader{
		GlobalSeq: globalSeq,
		Length:    length,
	}, nil
}
