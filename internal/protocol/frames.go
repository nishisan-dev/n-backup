// Package protocol implementa o protocolo binário NBackup para comunicação
// entre agent e server sobre TCP+TLS.
package protocol

import "errors"

// Magic bytes para identificação de frames.
var (
	MagicHandshake = [4]byte{'N', 'B', 'K', 'P'}
	MagicTrailer   = [4]byte{'D', 'O', 'N', 'E'}
	MagicPing      = [4]byte{'P', 'I', 'N', 'G'}
)

// ProtocolVersion é a versão atual do protocolo.
const ProtocolVersion byte = 0x01

// Status codes para ACK (Server → Client após Handshake).
const (
	StatusGo              byte = 0x00 // Pronto para receber
	StatusFull            byte = 0x01 // Disco cheio no destino
	StatusBusy            byte = 0x02 // Backup deste agent já em andamento
	StatusReject          byte = 0x03 // Agent não autorizado
	StatusStorageNotFound byte = 0x04 // Storage solicitado não existe
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
	Version     byte
	AgentName   string
	StorageName string
}

// ACK representa a resposta do server ao handshake.
type ACK struct {
	Status  byte
	Message string
}

// Trailer representa o frame de finalização enviado pelo client.
type Trailer struct {
	Checksum [32]byte // SHA-256
	Size     uint64   // Bytes transferidos
}

// FinalACK representa a resposta final do server após validação.
type FinalACK struct {
	Status byte
}

// HealthResponse representa a resposta do server ao health check.
type HealthResponse struct {
	Status   byte
	DiskFree uint64
}
