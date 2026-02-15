// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// maxChunkLength é o tamanho máximo aceitável de um chunk.
// Protege contra headers malformados que poderiam causar OOM.
// O valor é 2x o ChunkSize máximo configurável (16MB) como margem.
const maxChunkLength = 32 * 1024 * 1024 // 32MB

// ChunkAssembler gerencia chunks de streams paralelos por sessão.
// Implementa escrita incremental: chunks in-order são escritos direto no arquivo final.
// Chunks out-of-order são bufferizados em arquivos temporários individuais e
// descarregados assim que a lacuna de sequência é preenchida.
// Resultado: para o caso normal (single-stream ou round-robin sequencial),
// zero arquivos temporários são criados.
type ChunkAssembler struct {
	sessionID       string
	baseDir         string // diretório do agent
	outPath         string // caminho do arquivo de saída
	outFile         *os.File
	outBuf          *bufio.Writer
	chunkDir        string // subdir para chunks out-of-order
	chunkDirExists  bool   // lazy creation — só cria se necessário
	nextExpectedSeq uint32
	pendingChunks   map[uint32]pendingChunk // chunks out-of-order aguardando
	totalBytes      int64
	mu              sync.Mutex
	logger          *slog.Logger
}

// pendingChunk representa um chunk recebido fora de ordem, salvo em arquivo temporário.
type pendingChunk struct {
	filePath string
	length   int64
}

// NewChunkAssembler cria um assembler para uma sessão paralela.
// Abre o arquivo de saída imediatamente para escrita incremental.
func NewChunkAssembler(sessionID, agentDir string, logger *slog.Logger) (*ChunkAssembler, error) {
	outPath := filepath.Join(agentDir, fmt.Sprintf("assembled_%s.tmp", sessionID))
	outFile, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("creating output file: %w", err)
	}

	chunkDir := filepath.Join(agentDir, fmt.Sprintf("chunks_%s", sessionID))

	return &ChunkAssembler{
		sessionID:       sessionID,
		baseDir:         agentDir,
		outPath:         outPath,
		outFile:         outFile,
		outBuf:          bufio.NewWriterSize(outFile, 256*1024),
		chunkDir:        chunkDir,
		chunkDirExists:  false,
		nextExpectedSeq: 0,
		pendingChunks:   make(map[uint32]pendingChunk),
		logger:          logger,
	}, nil
}

// WriteChunk recebe um chunk com sua sequência global e dados.
// IMPORTANT: lê os dados do reader FORA do mutex para evitar que I/O TCP lento
// bloqueie o assembler inteiro. Apenas a escrita local (memória/disco) é protegida.
// - Se globalSeq == nextExpectedSeq → escreve direto no arquivo de saída + flush pendentes.
// - Se globalSeq > nextExpectedSeq → bufferiza em arquivo temporário (out-of-order).
func (ca *ChunkAssembler) WriteChunk(globalSeq uint32, data io.Reader, length int64) error {
	// Proteção contra OOM: rejeita chunks com tamanho absurdo (header malformado).
	if length <= 0 || length > maxChunkLength {
		return fmt.Errorf("chunk seq %d has invalid length %d (max %d)", globalSeq, length, maxChunkLength)
	}

	// Lê dados do TCP FORA do lock — operação potencialmente lenta.
	// Isso desacopla o I/O de rede do mutex, evitando que um stream lento
	// bloqueie todos os outros streams que tentam escrever.
	buf := make([]byte, length)
	if _, err := io.ReadFull(data, buf); err != nil {
		return fmt.Errorf("reading chunk seq %d from stream: %w", globalSeq, err)
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()

	if globalSeq == ca.nextExpectedSeq {
		// In-order: escreve direto no arquivo de saída (operação local, rápida)
		n, err := ca.outBuf.Write(buf)
		if err != nil {
			return fmt.Errorf("writing chunk seq %d to output: %w", globalSeq, err)
		}
		ca.totalBytes += int64(n)
		ca.nextExpectedSeq++

		ca.logger.Debug("chunk written in-order", "globalSeq", globalSeq, "bytes", n)

		// Flush pendentes contíguos
		return ca.flushPending()
	}

	if globalSeq < ca.nextExpectedSeq {
		// Chunk duplicado ou atrasado — ignora (dados já foram lidos acima, sem leak)
		ca.logger.Warn("ignoring duplicate/late chunk", "globalSeq", globalSeq, "expected", ca.nextExpectedSeq)
		return nil
	}

	// Out-of-order: salva em arquivo temporário
	return ca.saveOutOfOrder(globalSeq, buf)
}

// flushPending descarrega chunks pendentes contíguos no arquivo de saída.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) flushPending() error {
	for {
		pc, ok := ca.pendingChunks[ca.nextExpectedSeq]
		if !ok {
			break
		}

		// Abre o arquivo do chunk pendente
		f, err := os.Open(pc.filePath)
		if err != nil {
			return fmt.Errorf("opening pending chunk seq %d: %w", ca.nextExpectedSeq, err)
		}

		n, err := io.Copy(ca.outBuf, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("flushing pending chunk seq %d: %w", ca.nextExpectedSeq, err)
		}

		// Remove arquivo temporário
		os.Remove(pc.filePath)
		ca.totalBytes += n

		ca.logger.Debug("pending chunk flushed", "globalSeq", ca.nextExpectedSeq, "bytes", n)

		delete(ca.pendingChunks, ca.nextExpectedSeq)
		ca.nextExpectedSeq++
	}

	return nil
}

// saveOutOfOrder salva um chunk out-of-order em arquivo temporário.
// Recebe os dados já materializados em memória (lidos fora do mutex).
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) saveOutOfOrder(globalSeq uint32, data []byte) error {
	// Lazy creation do diretório de chunks
	if !ca.chunkDirExists {
		if err := os.MkdirAll(ca.chunkDir, 0755); err != nil {
			return fmt.Errorf("creating chunk directory: %w", err)
		}
		ca.chunkDirExists = true
	}

	name := fmt.Sprintf("chunk_%010d.tmp", globalSeq)
	path := filepath.Join(ca.chunkDir, name)

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating out-of-order chunk file seq %d: %w", globalSeq, err)
	}

	n, err := f.Write(data)
	f.Close()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("writing out-of-order chunk seq %d: %w", globalSeq, err)
	}
	if n != len(data) {
		os.Remove(path)
		return fmt.Errorf("short write on out-of-order chunk seq %d: wrote %d of %d bytes", globalSeq, n, len(data))
	}

	ca.pendingChunks[globalSeq] = pendingChunk{filePath: path, length: int64(n)}
	ca.logger.Debug("chunk saved out-of-order", "globalSeq", globalSeq, "bytes", n, "pending", len(ca.pendingChunks))

	return nil
}

// Finalize faz flush do buffer e fecha o arquivo de saída.
// Retorna o path do arquivo montado e o total de bytes escritos.
func (ca *ChunkAssembler) Finalize() (string, int64, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if len(ca.pendingChunks) > 0 {
		ca.logger.Warn("finalizing with pending out-of-order chunks",
			"pending", len(ca.pendingChunks),
			"nextExpected", ca.nextExpectedSeq,
		)
	}

	if err := ca.outBuf.Flush(); err != nil {
		return "", 0, fmt.Errorf("flushing output buffer: %w", err)
	}

	if err := ca.outFile.Close(); err != nil {
		return "", 0, fmt.Errorf("closing output file: %w", err)
	}

	ca.logger.Info("assembly finalized",
		"session", ca.sessionID,
		"totalBytes", ca.totalBytes,
		"nextExpectedSeq", ca.nextExpectedSeq,
	)

	return ca.outPath, ca.totalBytes, nil
}

// Cleanup remove o diretório de chunks out-of-order e o arquivo de saída (se falhou).
func (ca *ChunkAssembler) Cleanup() error {
	if ca.chunkDirExists {
		os.RemoveAll(ca.chunkDir)
	}
	return nil
}

// ChunkDir retorna o caminho do diretório de staging dos chunks out-of-order.
func (ca *ChunkAssembler) ChunkDir() string {
	return ca.chunkDir
}
