// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// Package protocol implementa o protocolo binário NBackup para comunicação
// entre agent e server sobre TCP+TLS.
package protocol

import "errors"

// Magic bytes para identificação de frames.
var (
	MagicHandshake    = [4]byte{'N', 'B', 'K', 'P'}
	MagicTrailer      = [4]byte{'D', 'O', 'N', 'E'}
	MagicPing         = [4]byte{'P', 'I', 'N', 'G'}
	MagicResume       = [4]byte{'R', 'S', 'M', 'E'}
	MagicSACK         = [4]byte{'S', 'A', 'C', 'K'}
	MagicParallelJoin = [4]byte{'P', 'J', 'I', 'N'}
	MagicChunkSACK    = [4]byte{'C', 'S', 'A', 'K'}
)

// ParallelACK status codes (Server → Client após ParallelJoin).
const (
	ParallelStatusOK       byte = 0x00 // Join aceito
	ParallelStatusFull     byte = 0x01 // Sessão não suporta mais streams
	ParallelStatusNotFound byte = 0x02 // SessionID não encontrado
)

// ParallelACK representa a resposta do server ao ParallelJoin.
// Formato: [Status 1B] [LastOffset uint64 8B]
// LastOffset indica quantos bytes o server já recebeu neste stream (0 para novo, >0 para resume).
type ParallelACK struct {
	Status     byte
	LastOffset uint64
}

// ProtocolVersion é a versão atual do protocolo.
const ProtocolVersion byte = 0x04

// Status codes para ACK (Server → Client após Handshake).
const (
	StatusGo              byte = 0x00 // Pronto para receber
	StatusFull            byte = 0x01 // Disco cheio no destino
	StatusBusy            byte = 0x02 // Backup deste agent já em andamento
	StatusReject          byte = 0x03 // Agent não autorizado
	StatusStorageNotFound byte = 0x04 // Storage solicitado não existe
)

// Status codes para Resume ACK (Server → Client após Resume).
const (
	ResumeStatusOK       byte = 0x00 // Resume aceito, continuar do offset
	ResumeStatusNotFound byte = 0x01 // Sessão não encontrada, reiniciar
)

// Status codes para Final ACK (Server → Client após Trailer).
const (
	FinalStatusOK               byte = 0x00 // Checksum válido, backup gravado
	FinalStatusChecksumMismatch byte = 0x01 // Hash não confere
	FinalStatusWriteError       byte = 0x02 // Erro de I/O no destino
)

// Health check status codes.
const (
	HealthStatusReady       byte = 0x00
	HealthStatusBusy        byte = 0x01
	HealthStatusLowDisk     byte = 0x02
	HealthStatusMaintenance byte = 0x03
)

// Erros do protocolo.
var (
	ErrInvalidMagic   = errors.New("protocol: invalid magic bytes")
	ErrInvalidVersion = errors.New("protocol: unsupported protocol version")
	ErrTruncatedFrame = errors.New("protocol: truncated frame")
)

// Handshake representa o frame de handshake enviado pelo client.
type Handshake struct {
	Version       byte
	AgentName     string
	StorageName   string
	BackupName    string
	ClientVersion string
}

// ACK representa a resposta do server ao handshake.
type ACK struct {
	Status          byte
	Message         string
	SessionID       string // UUID da sessão (gerado pelo server)
	CompressionMode byte   // Tipo de compressão negociado (v4+)
}

// Compression mode constants.
const (
	CompressionGzip byte = 0x00 // gzip (pgzip paralelo) — default
	CompressionZstd byte = 0x01 // zstd (klauspost/compress)
)

// Trailer representa o frame de finalização enviado pelo client.
type Trailer struct {
	Checksum [32]byte // SHA-256
	Size     uint64   // Bytes transferidos
}

// FinalACK representa a resposta final do server após validação.
type FinalACK struct {
	Status byte
}

// Resume representa o frame de resume enviado pelo client.
type Resume struct {
	SessionID   string
	AgentName   string
	StorageName string
}

// ResumeACK representa a resposta do server ao resume.
type ResumeACK struct {
	Status     byte
	LastOffset uint64
}

// SACK representa um selective acknowledgment do server (offset confirmado).
type SACK struct {
	Offset uint64
}

// HealthResponse representa a resposta do server ao health check.
type HealthResponse struct {
	Status   byte
	DiskFree uint64
}

// ParallelInit é enviado dentro do handshake para indicar suporte a streams paralelos.
// Incluído como extensão opcional do Handshake (Client → Server).
type ParallelInit struct {
	MaxStreams uint8  // Número máximo de streams paralelos (1-8)
	ChunkSize  uint32 // Tamanho de cada chunk em bytes
}

// Status codes para ParallelInitACK.
const (
	ParallelInitStatusOK    byte = 0x00
	ParallelInitStatusError byte = 0x01
)

// ParallelInitACK é enviado pelo server para confirmar que a sessão paralela foi inicializada.
type ParallelInitACK struct {
	Status byte
}

// ParallelJoin é enviado por conexões secundárias para se juntar a uma sessão existente.
// Formato: Magic "PJIN" [4B] [Version 1B] [SessionID UTF-8 '\n'] [StreamIndex uint8 1B]
type ParallelJoin struct {
	SessionID   string
	StreamIndex uint8
}

// ChunkSACK é o selective acknowledgment por stream (Server → Client).
// Formato: Magic "CSAK" [4B] [StreamIndex uint8 1B] [ChunkSeq uint32 4B] [Offset uint64 8B]
type ChunkSACK struct {
	StreamIndex uint8
	ChunkSeq    uint32
	Offset      uint64
}

// ChunkHeaderSize é o tamanho em bytes do ChunkHeader no wire: GlobalSeq(4B) + Length(4B).
const ChunkHeaderSize = 8

// ChunkHeader precede cada chunk no stream paralelo (Client → Server).
// Permite ao server reconstruir a ordem global dos chunks.
// Formato: [GlobalSeq uint32 4B] [Length uint32 4B]
type ChunkHeader struct {
	GlobalSeq uint32 // sequência global do chunk (0, 1, 2, ...)
	Length    uint32 // tamanho dos dados que seguem
}
