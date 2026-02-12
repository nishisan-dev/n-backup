// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ChunkMeta contém metadados de um chunk recebido.
type ChunkMeta struct {
	StreamIndex uint8
	ChunkSeq    uint32
	Offset      uint64
	Length      int64
}

// ChunkAssembler gerencia chunks de streams paralelos por sessão.
// Cada stream escreve em um chunk file separado.
// Ao final, concatena todos na ordem de ChunkSeq para produzir o arquivo final.
type ChunkAssembler struct {
	sessionID string
	baseDir   string // diretório do agent
	manifest  []ChunkMeta
	chunkDir  string // subdir para chunks desta sessão
	mu        sync.Mutex
	logger    *slog.Logger
}

// NewChunkAssembler cria um assembler para uma sessão paralela.
func NewChunkAssembler(sessionID, agentDir string, logger *slog.Logger) (*ChunkAssembler, error) {
	chunkDir := filepath.Join(agentDir, fmt.Sprintf("chunks_%s", sessionID))
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		return nil, fmt.Errorf("creating chunk directory: %w", err)
	}

	return &ChunkAssembler{
		sessionID: sessionID,
		baseDir:   agentDir,
		chunkDir:  chunkDir,
		manifest:  make([]ChunkMeta, 0),
		logger:    logger,
	}, nil
}

// ChunkFile retorna o arquivo para escrita do chunk de um stream específico.
// O arquivo é criado se não existir, ou aberto para append se já existir.
func (ca *ChunkAssembler) ChunkFile(streamIndex uint8) (*os.File, string, error) {
	name := fmt.Sprintf("chunk_%d.tmp", streamIndex)
	path := filepath.Join(ca.chunkDir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, "", fmt.Errorf("opening chunk file for stream %d: %w", streamIndex, err)
	}
	return f, path, nil
}

// RegisterChunk registra metadados de um chunk recebido.
func (ca *ChunkAssembler) RegisterChunk(meta ChunkMeta) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.manifest = append(ca.manifest, meta)
}

// Assemble concatena todos os chunk files na ordem de ChunkSeq,
// produzindo um único arquivo .tmp que pode ser validado e commitado.
// Retorna o path do arquivo montado.
func (ca *ChunkAssembler) Assemble() (string, int64, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Ordena por stream index (cada stream produz dados sequenciais)
	sort.Slice(ca.manifest, func(i, j int) bool {
		if ca.manifest[i].StreamIndex != ca.manifest[j].StreamIndex {
			return ca.manifest[i].StreamIndex < ca.manifest[j].StreamIndex
		}
		return ca.manifest[i].ChunkSeq < ca.manifest[j].ChunkSeq
	})

	// Cria arquivo final
	outPath := filepath.Join(ca.baseDir, fmt.Sprintf("assembled_%s.tmp", ca.sessionID))
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", 0, fmt.Errorf("creating assembled file: %w", err)
	}
	defer outFile.Close()

	var totalBytes int64

	// Lê e lista chunk files por stream index (0, 1, 2, ...)
	// Cada chunk file contém dados sequenciais de um stream
	entries, err := os.ReadDir(ca.chunkDir)
	if err != nil {
		return "", 0, fmt.Errorf("reading chunk directory: %w", err)
	}

	// Ordena para garantir concatenação na ordem correta
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		chunkPath := filepath.Join(ca.chunkDir, entry.Name())
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			return "", 0, fmt.Errorf("opening chunk %s: %w", entry.Name(), err)
		}

		n, err := io.Copy(outFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return "", 0, fmt.Errorf("copying chunk %s: %w", entry.Name(), err)
		}

		totalBytes += n
		ca.logger.Debug("assembled chunk", "file", entry.Name(), "bytes", n)
	}

	ca.logger.Info("assembly complete",
		"session", ca.sessionID,
		"totalBytes", totalBytes,
		"chunks", len(entries),
	)

	return outPath, totalBytes, nil
}

// Cleanup remove o diretório de chunks após a montagem.
func (ca *ChunkAssembler) Cleanup() error {
	return os.RemoveAll(ca.chunkDir)
}

// ChunkDir retorna o caminho do diretório de staging dos chunks.
func (ca *ChunkAssembler) ChunkDir() string {
	return ca.chunkDir
}
