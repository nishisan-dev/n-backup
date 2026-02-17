// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"hash"
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

// defaultPendingMemLimit é o limite de bytes para chunks out-of-order
// mantidos em memória antes de fazer spill para disco.
const defaultPendingMemLimit int64 = 8 * 1024 * 1024 // 8MB

const (
	// AssemblerModeEager monta chunks conforme chegam (com reordenação incremental).
	AssemblerModeEager = "eager"
	// AssemblerModeLazy persiste chunks recebidos e monta apenas no finalize.
	AssemblerModeLazy = "lazy"
)

// ChunkAssemblerOptions configura o comportamento do assembler.
type ChunkAssemblerOptions struct {
	Mode            string
	PendingMemLimit int64
}

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
	hasher          hash.Hash
	checksum        [32]byte
	finalized       bool
	chunkDir        string // subdir para chunks out-of-order
	chunkDirExists  bool   // lazy creation — só cria se necessário
	nextExpectedSeq uint32
	pendingChunks   map[uint32]pendingChunk // chunks out-of-order aguardando
	pendingMemBytes int64                   // bytes pendentes em memória
	pendingMemLimit int64                   // limite de pendência em memória
	mode            string
	lazyMaxSeq      uint32
	totalBytes      int64
	mu              sync.Mutex
	logger          *slog.Logger
}

// AssemblerStats contém métricas do estado atual do assembler.
type AssemblerStats struct {
	NextExpectedSeq uint32
	PendingChunks   int
	PendingMemBytes int64
	TotalBytes      int64
	Finalized       bool
}

// Stats retorna um snapshot das métricas do assembler.
func (ca *ChunkAssembler) Stats() AssemblerStats {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return AssemblerStats{
		NextExpectedSeq: ca.nextExpectedSeq,
		PendingChunks:   len(ca.pendingChunks),
		PendingMemBytes: ca.pendingMemBytes,
		TotalBytes:      ca.totalBytes,
		Finalized:       ca.finalized,
	}
}

// pendingChunk representa um chunk recebido fora de ordem.
// Pode estar em memória (data) ou em arquivo temporário (filePath).
type pendingChunk struct {
	data     []byte
	filePath string
	length   int64
}

// NewChunkAssembler cria um assembler para uma sessão paralela.
// Abre o arquivo de saída imediatamente para escrita incremental.
func NewChunkAssembler(sessionID, agentDir string, logger *slog.Logger) (*ChunkAssembler, error) {
	return NewChunkAssemblerWithOptions(sessionID, agentDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: defaultPendingMemLimit,
	})
}

// NewChunkAssemblerWithMemLimit cria um assembler com limite customizado de
// memória para chunks out-of-order (útil para testes e tunning).
func NewChunkAssemblerWithMemLimit(sessionID, agentDir string, logger *slog.Logger, pendingMemLimit int64) (*ChunkAssembler, error) {
	return NewChunkAssemblerWithOptions(sessionID, agentDir, logger, ChunkAssemblerOptions{
		Mode:            AssemblerModeEager,
		PendingMemLimit: pendingMemLimit,
	})
}

// NewChunkAssemblerWithOptions cria um assembler com modo configurável.
func NewChunkAssemblerWithOptions(sessionID, agentDir string, logger *slog.Logger, opts ChunkAssemblerOptions) (*ChunkAssembler, error) {
	mode := opts.Mode
	if mode == "" {
		mode = AssemblerModeEager
	}
	if mode != AssemblerModeEager && mode != AssemblerModeLazy {
		return nil, fmt.Errorf("invalid assembler mode %q", mode)
	}

	pendingMemLimit := opts.PendingMemLimit
	if mode == AssemblerModeEager && pendingMemLimit <= 0 {
		pendingMemLimit = defaultPendingMemLimit
	}

	outPath := filepath.Join(agentDir, fmt.Sprintf("assembled_%s.tmp", sessionID))
	outFile, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("creating output file: %w", err)
	}

	chunkDir := filepath.Join(agentDir, fmt.Sprintf("chunks_%s", sessionID))
	hasher := sha256.New()

	return &ChunkAssembler{
		sessionID:       sessionID,
		baseDir:         agentDir,
		outPath:         outPath,
		outFile:         outFile,
		outBuf:          bufio.NewWriterSize(io.MultiWriter(outFile, hasher), 1024*1024),
		hasher:          hasher,
		chunkDir:        chunkDir,
		chunkDirExists:  false,
		nextExpectedSeq: 0,
		pendingChunks:   make(map[uint32]pendingChunk),
		pendingMemLimit: pendingMemLimit,
		mode:            mode,
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

	if ca.mode == AssemblerModeLazy {
		return ca.writeChunkLazy(globalSeq, buf)
	}

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

// writeChunkLazy grava cada chunk em staging e posterga montagem para Finalize.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) writeChunkLazy(globalSeq uint32, buf []byte) error {
	if _, exists := ca.pendingChunks[globalSeq]; exists {
		ca.logger.Warn("ignoring duplicate chunk in lazy mode", "globalSeq", globalSeq)
		return nil
	}
	if len(ca.pendingChunks) == 0 || globalSeq > ca.lazyMaxSeq {
		ca.lazyMaxSeq = globalSeq
	}

	if !ca.chunkDirExists {
		if err := os.MkdirAll(ca.chunkDir, 0755); err != nil {
			return fmt.Errorf("creating chunk directory: %w", err)
		}
		ca.chunkDirExists = true
	}

	name := fmt.Sprintf("chunk_%010d.tmp", globalSeq)
	path := filepath.Join(ca.chunkDir, name)
	if err := os.WriteFile(path, buf, 0644); err != nil {
		return fmt.Errorf("writing lazy chunk seq %d: %w", globalSeq, err)
	}

	ca.pendingChunks[globalSeq] = pendingChunk{filePath: path, length: int64(len(buf))}
	ca.totalBytes += int64(len(buf))
	return nil
}

// flushPending descarrega chunks pendentes contíguos no arquivo de saída.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) flushPending() error {
	for {
		pc, ok := ca.pendingChunks[ca.nextExpectedSeq]
		if !ok {
			break
		}

		var n int64
		if pc.data != nil {
			// Pendente em memória: escreve direto no output.
			written, err := ca.outBuf.Write(pc.data)
			if err != nil {
				return fmt.Errorf("flushing in-memory pending chunk seq %d: %w", ca.nextExpectedSeq, err)
			}
			n = int64(written)
			ca.pendingMemBytes -= int64(len(pc.data))
			if ca.pendingMemBytes < 0 {
				ca.pendingMemBytes = 0
			}
		} else {
			// Pendente em disco: faz copy do arquivo temporário.
			f, err := os.Open(pc.filePath)
			if err != nil {
				return fmt.Errorf("opening pending chunk seq %d: %w", ca.nextExpectedSeq, err)
			}

			n, err = io.Copy(ca.outBuf, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("flushing pending chunk seq %d: %w", ca.nextExpectedSeq, err)
			}

			// Remove arquivo temporário
			os.Remove(pc.filePath)
		}
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
	// Prioriza memória para evitar write/read extra em discos lentos (ex: USB).
	if ca.pendingMemBytes+int64(len(data)) <= ca.pendingMemLimit {
		copyBuf := append([]byte(nil), data...)
		ca.pendingChunks[globalSeq] = pendingChunk{data: copyBuf, length: int64(len(copyBuf))}
		ca.pendingMemBytes += int64(len(copyBuf))
		ca.logger.Debug("chunk saved out-of-order in memory",
			"globalSeq", globalSeq,
			"bytes", len(copyBuf),
			"pending", len(ca.pendingChunks),
			"pendingMemBytes", ca.pendingMemBytes,
			"pendingMemLimit", ca.pendingMemLimit)
		return nil
	}

	// Excedeu limite de memória: faz spill para disco.
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
	ca.logger.Debug("chunk saved out-of-order on disk",
		"globalSeq", globalSeq,
		"bytes", n,
		"pending", len(ca.pendingChunks),
		"pendingMemBytes", ca.pendingMemBytes,
		"pendingMemLimit", ca.pendingMemLimit)

	return nil
}

// Finalize faz flush do buffer e fecha o arquivo de saída.
// Retorna o path do arquivo montado e o total de bytes escritos.
func (ca *ChunkAssembler) Finalize() (string, int64, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if ca.mode == AssemblerModeLazy {
		if err := ca.finalizeLazy(); err != nil {
			return "", 0, err
		}
	}

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
	copy(ca.checksum[:], ca.hasher.Sum(nil))
	ca.finalized = true

	ca.logger.Info("assembly finalized",
		"session", ca.sessionID,
		"totalBytes", ca.totalBytes,
		"nextExpectedSeq", ca.nextExpectedSeq,
	)

	return ca.outPath, ca.totalBytes, nil
}

// finalizeLazy monta os chunks staged em ordem de sequência e remove os temporários.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) finalizeLazy() error {
	if len(ca.pendingChunks) == 0 {
		return nil
	}

	for seq := uint32(0); seq <= ca.lazyMaxSeq; seq++ {
		pc, ok := ca.pendingChunks[seq]
		if !ok {
			return fmt.Errorf("missing chunk seq %d in lazy assembly", seq)
		}
		f, err := os.Open(pc.filePath)
		if err != nil {
			return fmt.Errorf("opening lazy chunk seq %d: %w", seq, err)
		}
		if _, err := io.Copy(ca.outBuf, f); err != nil {
			f.Close()
			return fmt.Errorf("flushing lazy chunk seq %d: %w", seq, err)
		}
		f.Close()
		os.Remove(pc.filePath)
		delete(ca.pendingChunks, seq)
	}

	return nil
}

// Checksum retorna o SHA-256 do arquivo montado.
// Só é válido após Finalize.
func (ca *ChunkAssembler) Checksum() ([32]byte, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if !ca.finalized {
		var zero [32]byte
		return zero, fmt.Errorf("assembly checksum unavailable before finalize")
	}
	return ca.checksum, nil
}

// Cleanup remove o diretório de chunks out-of-order e o arquivo de saída (se falhou).
func (ca *ChunkAssembler) Cleanup() error {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Se não foi finalizado, fecha e remove o arquivo de saída parcial
	if ca.outFile != nil && !ca.finalized {
		ca.outFile.Close()
		ca.outFile = nil
		os.Remove(ca.outPath)
	}

	if ca.chunkDirExists {
		os.RemoveAll(ca.chunkDir)
	}
	ca.pendingChunks = make(map[uint32]pendingChunk)
	ca.pendingMemBytes = 0
	return nil
}

// ChunkDir retorna o caminho do diretório de staging dos chunks out-of-order.
func (ca *ChunkAssembler) ChunkDir() string {
	return ca.chunkDir
}
