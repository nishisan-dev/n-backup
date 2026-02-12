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
	// Remove o delimitador '\n'
	msg = msg[:len(msg)-1]

	return &ACK{
		Status:  status[0],
		Message: msg,
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
