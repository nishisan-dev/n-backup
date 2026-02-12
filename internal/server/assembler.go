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
	GlobalSeq   uint32 // sequência global para reconstrução
	FilePath    string // caminho do arquivo de chunk no disco
	Length      int64
}

// ChunkAssembler gerencia chunks de streams paralelos por sessão.
// Cada chunk é escrito em um arquivo separado com nome baseado no GlobalSeq.
// Ao final, concatena todos na ordem de GlobalSeq para produzir o arquivo final.
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

// ChunkFileForSeq retorna o arquivo para escrita de um chunk com sequência global.
func (ca *ChunkAssembler) ChunkFileForSeq(globalSeq uint32) (*os.File, string, error) {
	name := fmt.Sprintf("chunk_%010d.tmp", globalSeq)
	path := filepath.Join(ca.chunkDir, name)

	f, err := os.Create(path)
	if err != nil {
		return nil, "", fmt.Errorf("creating chunk file for seq %d: %w", globalSeq, err)
	}
	return f, path, nil
}

// RegisterChunk registra metadados de um chunk recebido.
func (ca *ChunkAssembler) RegisterChunk(meta ChunkMeta) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.manifest = append(ca.manifest, meta)
}

// Assemble concatena todos os chunk files na ordem de GlobalSeq,
// produzindo um único arquivo .tmp que pode ser validado e commitado.
// Retorna o path do arquivo montado.
func (ca *ChunkAssembler) Assemble() (string, int64, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Ordena por GlobalSeq — garante ordem original dos dados
	sort.Slice(ca.manifest, func(i, j int) bool {
		return ca.manifest[i].GlobalSeq < ca.manifest[j].GlobalSeq
	})

	// Cria arquivo final
	outPath := filepath.Join(ca.baseDir, fmt.Sprintf("assembled_%s.tmp", ca.sessionID))
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", 0, fmt.Errorf("creating assembled file: %w", err)
	}
	defer outFile.Close()

	var totalBytes int64

	for _, meta := range ca.manifest {
		chunkFile, err := os.Open(meta.FilePath)
		if err != nil {
			return "", 0, fmt.Errorf("opening chunk seq %d: %w", meta.GlobalSeq, err)
		}

		n, err := io.Copy(outFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return "", 0, fmt.Errorf("copying chunk seq %d: %w", meta.GlobalSeq, err)
		}

		totalBytes += n
		ca.logger.Debug("assembled chunk", "globalSeq", meta.GlobalSeq, "stream", meta.StreamIndex, "bytes", n)
	}

	ca.logger.Info("assembly complete",
		"session", ca.sessionID,
		"totalBytes", totalBytes,
		"chunks", len(ca.manifest),
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
