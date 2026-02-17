// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// StreamResult contém o resultado de uma operação de streaming.
type StreamResult struct {
	Checksum [32]byte
	Size     uint64
}

// Stream executa o pipeline de streaming zero-copy:
// Scanner → tar.Writer → compressor(gzip|zstd) → io.Writer (conexão de rede).
// O SHA-256 é calculado inline sobre o stream compactado.
// Se progress não for nil, alimenta contadores de bytes e objetos.
// Se onObject não for nil, é chamado após cada objeto processado (usado para contadores externos).
// Retorna o checksum e total de bytes escritos no destino.
func Stream(ctx context.Context, scanner *Scanner, dest io.Writer, progress *ProgressReporter, onObject func(), compressionMode byte) (*StreamResult, error) {
	// Buffer de escrita para reduzir syscalls na conexão TLS
	bufDest := bufio.NewWriterSize(dest, 256*1024) // 256KB

	// Cria o hash inline
	hasher := sha256.New()
	counter := &countWriter{w: io.MultiWriter(bufDest, hasher), progress: progress}

	// Cria compressor com base no modo negociado
	compressor, err := newCompressor(counter, compressionMode)
	if err != nil {
		return nil, err
	}

	tw := tar.NewWriter(compressor)

	// Itera sobre os arquivos via scanner
	scanErr := scanner.Scan(ctx, func(entry FileEntry) error {
		// Verifica cancelamento
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := addToTar(tw, entry); err != nil {
			return err
		}
		if progress != nil {
			progress.AddObject()
		}
		if onObject != nil {
			onObject()
		}
		return nil
	})

	if scanErr != nil {
		tw.Close()
		compressor.Close()
		return nil, fmt.Errorf("scanning files: %w", scanErr)
	}

	// Fecha o tar writer (escreve os trailers)
	if err := tw.Close(); err != nil {
		compressor.Close()
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}

	// Fecha o compressor (flush + trailer)
	if err := compressor.Close(); err != nil {
		return nil, fmt.Errorf("closing compressor: %w", err)
	}

	// Flush do buffer para a conexão
	if err := bufDest.Flush(); err != nil {
		return nil, fmt.Errorf("flushing buffer: %w", err)
	}

	var checksum [32]byte
	copy(checksum[:], hasher.Sum(nil))

	return &StreamResult{
		Checksum: checksum,
		Size:     counter.n,
	}, nil
}

// newCompressor cria um io.WriteCloser para compressão com base no mode.
func newCompressor(w io.Writer, mode byte) (io.WriteCloser, error) {
	switch mode {
	case protocol.CompressionZstd:
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault))
	default: // CompressionGzip
		gzWriter, err := pgzip.NewWriterLevel(w, pgzip.BestSpeed)
		if err != nil {
			return nil, fmt.Errorf("creating gzip writer: %w", err)
		}
		if err := gzWriter.SetConcurrency(1<<20, runtime.GOMAXPROCS(0)); err != nil {
			return nil, fmt.Errorf("configuring gzip concurrency: %w", err)
		}
		return gzWriter, nil
	}
}

// addToTar adiciona um arquivo ou diretório ao tar archive.
// Para arquivos regulares, usa stat do fd aberto + LimitReader para evitar
// "write too long" em arquivos que crescem durante o backup (ex: logs ativos).
func addToTar(tw *tar.Writer, entry FileEntry) error {
	// Trata symlinks
	link := ""
	if entry.Info.Mode()&os.ModeSymlink != 0 {
		var err error
		link, err = os.Readlink(entry.Path)
		if err != nil {
			return nil // pula symlinks quebrados
		}
	}

	// Se for arquivo regular, abre antes de criar o header
	// para garantir consistência entre size no header e bytes copiados
	if entry.Info.Mode().IsRegular() {
		f, err := os.Open(entry.Path)
		if err != nil {
			return nil // pula arquivos que sumiram entre scan e tar
		}
		defer f.Close()

		// Stat via fd aberto (não via path) — evita TOCTOU
		fi, err := f.Stat()
		if err != nil {
			return nil // pula se não conseguir stat
		}

		header, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("creating tar header for %s: %w", entry.Path, err)
		}
		header.Name = entry.RelPath

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", entry.Path, err)
		}

		// LimitReader garante que nunca escrevemos mais que o declarado no header
		if _, err := io.Copy(tw, io.LimitReader(f, fi.Size())); err != nil {
			return fmt.Errorf("writing file %s to tar: %w", entry.Path, err)
		}

		return nil
	}

	// Diretórios e symlinks
	header, err := tar.FileInfoHeader(entry.Info, link)
	if err != nil {
		return fmt.Errorf("creating tar header for %s: %w", entry.Path, err)
	}
	header.Name = entry.RelPath

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", entry.Path, err)
	}

	return nil
}

// countWriter conta os bytes escritos e opcionalmente alimenta o progress reporter.
type countWriter struct {
	w        io.Writer
	n        uint64
	progress *ProgressReporter
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += uint64(n)
	if cw.progress != nil {
		cw.progress.AddBytes(int64(n))
	}
	return n, err
}

// hashReader é um wrapper para io.Reader que calcula hash inline.
// Não utilizado diretamente na pipeline atual, mas disponível para o server.
var _ hash.Hash = sha256.New()
