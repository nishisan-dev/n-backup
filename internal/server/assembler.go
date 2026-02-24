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
	"sync/atomic"
	"time"
)

// maxChunkLength é o tamanho máximo aceitável de um chunk.
// Protege contra headers malformados que poderiam causar OOM.
// O valor é 2x o ChunkSize máximo configurável (16MB) como margem.
const maxChunkLength = 32 * 1024 * 1024 // 32MB

// defaultPendingMemLimit é o limite de bytes para chunks out-of-order
// mantidos em memória antes de fazer spill para disco.
const defaultPendingMemLimit int64 = 8 * 1024 * 1024 // 8MB

// chunkShardFanout define quantos subdiretórios por nível usamos para
// distribuir arquivos de chunk no staging (fanout^levels shards possíveis).
const chunkShardFanout uint32 = 256

const (
	// AssemblerModeEager monta chunks conforme chegam (com reordenação incremental).
	AssemblerModeEager = "eager"
	// AssemblerModeLazy persiste chunks recebidos e monta apenas no finalize.
	AssemblerModeLazy = "lazy"
)

// syncFile permite override em testes para validar paths com fsync.
var syncFile = func(f *os.File) error {
	return f.Sync()
}

// ChunkAssemblerOptions configura o comportamento do assembler.
type ChunkAssemblerOptions struct {
	Mode             string
	PendingMemLimit  int64
	ShardLevels      int  // 1 ou 2 (default: 1)
	FsyncChunkWrites bool // true = fsync a cada write de chunk em staging
}

// ChunkAssembler gerencia chunks de streams paralelos por sessão.
// Implementa escrita incremental: chunks in-order são escritos direto no arquivo final.
// Chunks out-of-order são bufferizados em arquivos temporários individuais e
// descarregados assim que a lacuna de sequência é preenchida.
// Resultado: para o caso normal (single-stream ou round-robin sequencial),
// zero arquivos temporários são criados.
type ChunkAssembler struct {
	sessionID        string
	baseDir          string // diretório do agent
	outPath          string // caminho do arquivo de saída
	outFile          *os.File
	outBuf           *bufio.Writer
	hasher           hash.Hash
	checksum         [32]byte
	chunkDir         string                  // subdir para chunks out-of-order
	chunkDirExists   bool                    // lazy creation — só cria se necessário
	pendingChunks    map[uint32]pendingChunk // chunks out-of-order aguardando (protegido por mu)
	pendingMemLimit  int64                   // limite de pendência em memória (imutável)
	mode             string                  // assembler mode (imutável)
	shardLevels      int                     // 1 ou 2 níveis de sharding (imutável)
	fsyncChunkWrites bool                    // fsync em writes de chunk staging (imutável)
	createdShards    map[string]struct{}     // cache de diretórios de shard já criados
	mu               sync.Mutex              // protege pendingChunks, outBuf, outFile, chunkDirExists, createdShards
	logger           *slog.Logger

	// Campos atômicos — lidos por Stats() sem lock.
	// Escritos sob ca.mu pelos métodos de mutação.
	nextExpectedSeq   atomic.Uint32 // próximo seq esperado (in-order)
	pendingMemBytes   atomic.Int64  // bytes pendentes em memória
	totalBytes        atomic.Int64  // total de bytes escritos no output
	finalized         atomic.Bool   // true após Finalize() completar
	pendingCount      atomic.Int32  // len(pendingChunks) mantido via atomic
	lazyMaxSeq        atomic.Uint32 // maior seq recebido em lazy mode
	assembling        atomic.Bool   // true durante finalizeLazy()
	assembledChunks   atomic.Uint32 // chunks já montados no finalize lazy
	assemblyStartedAt atomic.Value  // time.Time — quando finalizeLazy() iniciou
}

// AssemblerStats contém métricas do estado atual do assembler.
type AssemblerStats struct {
	NextExpectedSeq   uint32
	PendingChunks     int
	PendingMemBytes   int64
	TotalBytes        int64
	Finalized         bool
	TotalChunks       uint32    // total de chunks a montar (lazy: lazyMaxSeq+1, eager: nextExpectedSeq)
	AssembledChunks   uint32    // chunks já montados no finalize (relevante para lazy)
	Phase             string    // "receiving" | "assembling" | "done"
	AssemblyStartedAt time.Time // quando o assembly lazy iniciou (zero se não aplicável)
}

// Stats retorna um snapshot das métricas do assembler.
// Lock-free: lê apenas campos atômicos para evitar contention com Finalize().
func (ca *ChunkAssembler) Stats() AssemblerStats {
	finalized := ca.finalized.Load()
	nextSeq := ca.nextExpectedSeq.Load()
	pending := int(ca.pendingCount.Load())
	pendingMem := ca.pendingMemBytes.Load()
	totalBytes := ca.totalBytes.Load()
	lazyMax := ca.lazyMaxSeq.Load()
	assembling := ca.assembling.Load()
	assembled := ca.assembledChunks.Load()
	var assemblyStartedAt time.Time
	if v := ca.assemblyStartedAt.Load(); v != nil {
		assemblyStartedAt = v.(time.Time)
	}

	var phase string
	var totalChunks uint32
	switch {
	case finalized:
		phase = "done"
		if ca.mode == AssemblerModeLazy {
			totalChunks = lazyMax + 1
		} else {
			totalChunks = nextSeq
		}
		assembled = totalChunks
	case assembling:
		phase = "assembling"
		totalChunks = lazyMax + 1
	default:
		phase = "receiving"
		if ca.mode == AssemblerModeLazy {
			totalChunks = lazyMax + 1
		} else {
			totalChunks = nextSeq
		}
	}

	return AssemblerStats{
		NextExpectedSeq:   nextSeq,
		PendingChunks:     pending,
		PendingMemBytes:   pendingMem,
		TotalBytes:        totalBytes,
		Finalized:         finalized,
		TotalChunks:       totalChunks,
		AssembledChunks:   assembled,
		Phase:             phase,
		AssemblyStartedAt: assemblyStartedAt,
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

	shardLevels := opts.ShardLevels
	if shardLevels == 0 {
		shardLevels = 1
	}
	if shardLevels < 1 || shardLevels > 2 {
		return nil, fmt.Errorf("invalid shard levels %d (must be 1 or 2)", shardLevels)
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

	ca := &ChunkAssembler{
		sessionID:        sessionID,
		baseDir:          agentDir,
		outPath:          outPath,
		outFile:          outFile,
		outBuf:           bufio.NewWriterSize(io.MultiWriter(outFile, hasher), 1024*1024),
		hasher:           hasher,
		chunkDir:         chunkDir,
		chunkDirExists:   false,
		pendingChunks:    make(map[uint32]pendingChunk),
		pendingMemLimit:  pendingMemLimit,
		mode:             mode,
		shardLevels:      shardLevels,
		fsyncChunkWrites: opts.FsyncChunkWrites,
		createdShards:    make(map[string]struct{}),
		logger:           logger,
	}
	ca.nextExpectedSeq.Store(0)
	return ca, nil
}

// WriteChunk recebe um chunk com sua sequência global e dados.
// IMPORTANT: lê os dados do reader FORA do mutex para evitar que I/O TCP lento
// bloqueie o assembler inteiro. Apenas a escrita local (memória/disco) é protegida.
// - Se globalSeq == nextExpectedSeq → escreve direto no arquivo de saída + flush pendentes.
// - Se globalSeq > nextExpectedSeq → bufferiza em arquivo temporário (out-of-order).
//
// Nota sobre o lock: usa unlocks explícitos (não defer) para permitir que
// saveOutOfOrder libere/readquira ca.mu durante I/O de disco sem reentrada.
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

	if ca.mode == AssemblerModeLazy {
		err := ca.writeChunkLazy(globalSeq, buf)
		ca.mu.Unlock()
		return err
	}

	nextSeq := ca.nextExpectedSeq.Load()
	if globalSeq == nextSeq {
		// In-order: escreve direto no arquivo de saída (operação local, rápida)
		n, err := ca.outBuf.Write(buf)
		if err != nil {
			ca.mu.Unlock()
			return fmt.Errorf("writing chunk seq %d to output: %w", globalSeq, err)
		}
		ca.totalBytes.Add(int64(n))
		ca.nextExpectedSeq.Store(nextSeq + 1)

		ca.logger.Debug("chunk written in-order", "globalSeq", globalSeq, "bytes", n)

		// Flush pendentes contíguos
		err = ca.flushPending()
		ca.mu.Unlock()
		return err
	}

	if globalSeq < nextSeq {
		// Chunk duplicado ou atrasado — ignora (dados já foram lidos acima, sem leak)
		ca.logger.Warn("ignoring duplicate/late chunk", "globalSeq", globalSeq, "expected", nextSeq)
		ca.mu.Unlock()
		return nil
	}

	// Out-of-order: salva em arquivo temporário.
	// saveOutOfOrder é chamado com ca.mu held e retorna com ca.mu held.
	// No path de spill em disco, pode liberar/readquirir ca.mu internamente.
	err := ca.saveOutOfOrder(globalSeq, buf)
	ca.mu.Unlock()
	return err
}

// writeChunkLazy grava cada chunk em staging e posterga montagem para Finalize.
// Deve ser chamado com ca.mu held.
//
// IMPORTANTE: lazyMaxSeq é atualizado SOMENTE após o chunk ter sido gravado em
// disco e registrado em pendingChunks com sucesso. Atualizar antes exporia uma
// janela onde finalizeLazy() iteraria até um seq que não existe no mapa,
// retornando "missing chunk seq N in lazy assembly" mesmo sem perda de dados —
// apenas por uma falha de I/O transitória ou inode esgotado durante writeChunkFile.
func (ca *ChunkAssembler) writeChunkLazy(globalSeq uint32, buf []byte) error {
	if _, exists := ca.pendingChunks[globalSeq]; exists {
		ca.logger.Warn("ignoring duplicate chunk in lazy mode", "globalSeq", globalSeq)
		return nil
	}

	path, err := ca.chunkPath(globalSeq)
	if err != nil {
		return err
	}
	if err := writeChunkFile(path, buf, ca.fsyncChunkWrites); err != nil {
		return fmt.Errorf("writing lazy chunk seq %d: %w", globalSeq, err)
	}

	// Registra no mapa antes de atualizar lazyMaxSeq: garante que todo seq
	// contabilizado como "máximo recebido" possui entrada correspondente no mapa.
	ca.pendingChunks[globalSeq] = pendingChunk{filePath: path, length: int64(len(buf))}
	if len(ca.pendingChunks) == 1 || globalSeq > ca.lazyMaxSeq.Load() {
		ca.lazyMaxSeq.Store(globalSeq)
	}
	ca.pendingCount.Add(1)
	ca.totalBytes.Add(int64(len(buf)))
	return nil
}

// flushPending descarrega chunks pendentes contíguos no arquivo de saída.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) flushPending() error {
	for {
		nextSeq := ca.nextExpectedSeq.Load()
		pc, ok := ca.pendingChunks[nextSeq]
		if !ok {
			break
		}

		var n int64
		if pc.data != nil {
			// Pendente em memória: escreve direto no output.
			written, err := ca.outBuf.Write(pc.data)
			if err != nil {
				return fmt.Errorf("flushing in-memory pending chunk seq %d: %w", nextSeq, err)
			}
			n = int64(written)
			newMem := ca.pendingMemBytes.Add(-int64(len(pc.data)))
			if newMem < 0 {
				ca.pendingMemBytes.Store(0)
			}
		} else {
			// Pendente em disco: faz copy do arquivo temporário.
			f, err := os.Open(pc.filePath)
			if err != nil {
				return fmt.Errorf("opening pending chunk seq %d: %w", nextSeq, err)
			}

			n, err = io.Copy(ca.outBuf, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("flushing pending chunk seq %d: %w", nextSeq, err)
			}

			// Remove arquivo temporário
			os.Remove(pc.filePath)
		}
		ca.totalBytes.Add(n)

		ca.logger.Debug("pending chunk flushed", "globalSeq", nextSeq, "bytes", n)

		delete(ca.pendingChunks, nextSeq)
		ca.pendingCount.Add(-1)
		ca.nextExpectedSeq.Store(nextSeq + 1)
	}

	return nil
}

// saveOutOfOrder salva um chunk out-of-order com o padrão write-temp + commit atômico.
// Recebe os dados já materializados em memória (lidos fora do mutex).
// Deve ser chamado com ca.mu held e retorna com ca.mu held.
//
// Para chunks que precisam de spill em disco:
//  1. Calcula path final (sob lock) — garante que o shard dir existe via createdShards.
//  2. Escreve em arquivo temporário FORA do lock (I/O de disco livre de contenção).
//  3. Re-adquire o lock e revalida o estado do globalSeq (três casos possíveis).
//  4. Rename atômico do temp para o path final (sob lock).
func (ca *ChunkAssembler) saveOutOfOrder(globalSeq uint32, data []byte) error {
	currentMem := ca.pendingMemBytes.Load()

	// Caminho rápido: cabe em memória — sem I/O, permanece sob lock.
	if currentMem+int64(len(data)) <= ca.pendingMemLimit {
		copyBuf := append([]byte(nil), data...)
		ca.pendingChunks[globalSeq] = pendingChunk{data: copyBuf, length: int64(len(copyBuf))}
		ca.pendingCount.Add(1)
		newMem := ca.pendingMemBytes.Add(int64(len(copyBuf)))
		ca.logger.Debug("chunk saved out-of-order in memory",
			"globalSeq", globalSeq,
			"bytes", len(copyBuf),
			"pending", ca.pendingCount.Load(),
			"pendingMemBytes", newMem,
			"pendingMemLimit", ca.pendingMemLimit)
		return nil
	}

	// Caminho de spill em disco.

	// Passo 1 (sob lock): calcula o path final e garante que o shard dir existe.
	// chunkPath usa cache createdShards para evitar os.MkdirAll repetido.
	finalPath, err := ca.chunkPath(globalSeq)
	if err != nil {
		return err
	}
	shardDir := filepath.Dir(finalPath) // shard dir já criado pelo passo acima

	// Passo 2: escreve arquivo temporário FORA do lock.
	// shardDir existe (garantido pelo passo 1), portanto CreateTemp não falha por dir ausente.
	ca.mu.Unlock()
	tmpPath, writeErr := writeChunkToTemp(shardDir, data, ca.fsyncChunkWrites)
	ca.mu.Lock() // readquire antes de qualquer acesso ao estado

	if writeErr != nil {
		return writeErr
	}

	// Passo 3: revalidação após re-lock.
	// Enquanto o lock estava liberado, outra goroutine pode ter modificado o estado.
	// Três casos possíveis para globalSeq:

	// (a) Já registrado em pendingChunks — duplicata concorrente: descarta o temp.
	if _, exists := ca.pendingChunks[globalSeq]; exists {
		os.Remove(tmpPath)
		ca.logger.Warn("spill chunk registered concurrently, discarding temp", "globalSeq", globalSeq)
		return nil
	}

	currentNext := ca.nextExpectedSeq.Load()

	// (b) Chunk virou late/duplicata durante o I/O externo: descarta o temp.
	if globalSeq < currentNext {
		os.Remove(tmpPath)
		ca.logger.Warn("spill chunk became late during disk I/O, discarding",
			"globalSeq", globalSeq, "nextExpected", currentNext)
		return nil
	}

	// (c) Chunk virou in-order enquanto esperava o I/O: promove direto ao outBuf.
	// Os dados ainda estão em memória (parâmetro data), evitando uma leitura extra em disco.
	if globalSeq == currentNext {
		os.Remove(tmpPath)
		n, err := ca.outBuf.Write(data)
		if err != nil {
			return fmt.Errorf("writing promoted spill chunk seq %d: %w", globalSeq, err)
		}
		ca.totalBytes.Add(int64(n))
		ca.nextExpectedSeq.Store(currentNext + 1)
		ca.logger.Debug("spill chunk promoted to in-order after re-lock", "globalSeq", globalSeq, "bytes", n)
		return ca.flushPending()
	}

	// Passo 4: globalSeq ainda é out-of-order — commit atômico sob lock.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("committing spill chunk seq %d: %w", globalSeq, err)
	}
	ca.pendingChunks[globalSeq] = pendingChunk{filePath: finalPath, length: int64(len(data))}
	ca.pendingCount.Add(1)
	ca.logger.Debug("chunk saved out-of-order on disk",
		"globalSeq", globalSeq,
		"bytes", len(data),
		"pending", ca.pendingCount.Load(),
		"pendingMemBytes", ca.pendingMemBytes.Load(),
		"pendingMemLimit", ca.pendingMemLimit)
	return nil
}

// writeChunkFile cria/trunca um arquivo no path, escreve data e fecha.
// Quando fsyncEnabled=true, força Sync() antes do close para persistir chunk staging.
func writeChunkFile(path string, data []byte, fsyncEnabled bool) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating chunk file: %w", err)
	}
	n, err := f.Write(data)
	if err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("writing chunk file: %w", err)
	}
	if n != len(data) {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("short write on chunk file: wrote %d of %d bytes", n, len(data))
	}
	if fsyncEnabled {
		if err := syncFile(f); err != nil {
			f.Close()
			os.Remove(path)
			return fmt.Errorf("syncing chunk file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("closing chunk file: %w", err)
	}
	return nil
}

// writeChunkToTemp cria um arquivo temporário em dir, escreve data e fecha.
// Chamado sem ca.mu held. Retorna o path do arquivo temporário criado.
func writeChunkToTemp(dir string, data []byte, fsyncEnabled bool) (string, error) {
	f, err := os.CreateTemp(dir, "spill-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating spill temp file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("writing spill temp file: %w", err)
	}
	if fsyncEnabled {
		if err := syncFile(f); err != nil {
			f.Close()
			os.Remove(name)
			return "", fmt.Errorf("syncing spill temp file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("closing spill temp file: %w", err)
	}
	return name, nil
}

// chunkPath retorna o caminho de staging do chunk usando directory sharding
// de 1 ou 2 níveis, conforme configurado em shardLevels.
// Usa cache interno para evitar syscalls os.MkdirAll repetidas.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) chunkPath(globalSeq uint32) (string, error) {
	level1 := fmt.Sprintf("%02x", globalSeq%chunkShardFanout)
	var shardDir string
	if ca.shardLevels == 2 {
		level2 := fmt.Sprintf("%02x", (globalSeq/chunkShardFanout)%chunkShardFanout)
		shardDir = filepath.Join(ca.chunkDir, level1, level2)
	} else {
		shardDir = filepath.Join(ca.chunkDir, level1)
	}

	if _, exists := ca.createdShards[shardDir]; !exists {
		if err := os.MkdirAll(shardDir, 0755); err != nil {
			return "", fmt.Errorf("creating chunk shard directory: %w", err)
		}
		ca.createdShards[shardDir] = struct{}{}
		ca.chunkDirExists = true
	}

	name := fmt.Sprintf("chunk_%010d.tmp", globalSeq)
	return filepath.Join(shardDir, name), nil
}

// Finalize faz flush do buffer e fecha o arquivo de saída.
// Retorna o path do arquivo montado e o total de bytes escritos.
func (ca *ChunkAssembler) Finalize() (string, int64, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if ca.mode == AssemblerModeLazy {
		ca.assemblyStartedAt.Store(time.Now())
		ca.assembling.Store(true)
		defer ca.assembling.Store(false)
		if err := ca.finalizeLazy(); err != nil {
			return "", 0, err
		}
	}

	pendingCnt := ca.pendingCount.Load()
	if pendingCnt > 0 {
		ca.logger.Warn("finalizing with pending out-of-order chunks",
			"pending", pendingCnt,
			"nextExpected", ca.nextExpectedSeq.Load(),
		)
	}

	if err := ca.outBuf.Flush(); err != nil {
		return "", 0, fmt.Errorf("flushing output buffer: %w", err)
	}

	if err := ca.outFile.Close(); err != nil {
		return "", 0, fmt.Errorf("closing output file: %w", err)
	}
	copy(ca.checksum[:], ca.hasher.Sum(nil))
	ca.finalized.Store(true)

	total := ca.totalBytes.Load()
	ca.logger.Info("assembly finalized",
		"session", ca.sessionID,
		"totalBytes", total,
		"nextExpectedSeq", ca.nextExpectedSeq.Load(),
	)

	return ca.outPath, total, nil
}

// finalizeLazy monta os chunks staged em ordem de sequência e remove os temporários.
// Deve ser chamado com ca.mu held.
func (ca *ChunkAssembler) finalizeLazy() error {
	if len(ca.pendingChunks) == 0 {
		return nil
	}

	lazyMax := ca.lazyMaxSeq.Load()
	for seq := uint32(0); seq <= lazyMax; seq++ {
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
		ca.pendingCount.Add(-1)
		ca.assembledChunks.Add(1)
	}

	return nil
}

// Checksum retorna o SHA-256 do arquivo montado.
// Só é válido após Finalize.
func (ca *ChunkAssembler) Checksum() ([32]byte, error) {
	if !ca.finalized.Load() {
		var zero [32]byte
		return zero, fmt.Errorf("assembly checksum unavailable before finalize")
	}
	// checksum é escrito uma vez em Finalize() antes de finalized.Store(true).
	// Portanto é safe ler sem lock após finalized == true (happens-before).
	return ca.checksum, nil
}

// Cleanup remove o diretório de chunks out-of-order e o arquivo de saída (se falhou).
func (ca *ChunkAssembler) Cleanup() error {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Se não foi finalizado, fecha e remove o arquivo de saída parcial
	if ca.outFile != nil && !ca.finalized.Load() {
		ca.outFile.Close()
		ca.outFile = nil
		os.Remove(ca.outPath)
	}

	if ca.chunkDirExists {
		os.RemoveAll(ca.chunkDir)
	}
	ca.pendingChunks = make(map[uint32]pendingChunk)
	ca.pendingCount.Store(0)
	ca.pendingMemBytes.Store(0)
	return nil
}

// ChunkDir retorna o caminho do diretório de staging dos chunks out-of-order.
func (ca *ChunkAssembler) ChunkDir() string {
	return ca.chunkDir
}
