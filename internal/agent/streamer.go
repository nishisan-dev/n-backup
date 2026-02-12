// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
)

// StreamResult contém o resultado de uma operação de streaming.
type StreamResult struct {
	Checksum [32]byte
	Size     uint64
}

// Stream executa o pipeline de streaming zero-copy:
// Scanner → tar.Writer → gzip.Writer → io.Writer (conexão de rede).
// O SHA-256 é calculado inline sobre o stream gzip compactado.
// Retorna o checksum e total de bytes escritos no destino.
func Stream(ctx context.Context, scanner *Scanner, dest io.Writer) (*StreamResult, error) {
	// Buffer de escrita para reduzir syscalls na conexão TLS
	bufDest := bufio.NewWriterSize(dest, 256*1024) // 256KB

	// Cria o hash inline
	hasher := sha256.New()
	counter := &countWriter{w: io.MultiWriter(bufDest, hasher)}

	// Pipeline: tar → gzip → buffer → (dest + hasher)
	gzWriter, err := gzip.NewWriterLevel(counter, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("creating gzip writer: %w", err)
	}

	tw := tar.NewWriter(gzWriter)

	// Itera sobre os arquivos via scanner
	scanErr := scanner.Scan(ctx, func(entry FileEntry) error {
		// Verifica cancelamento
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		return addToTar(tw, entry)
	})

	if scanErr != nil {
		tw.Close()
		gzWriter.Close()
		return nil, fmt.Errorf("scanning files: %w", scanErr)
	}

	// Fecha o tar writer (escreve os trailers)
	if err := tw.Close(); err != nil {
		gzWriter.Close()
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}

	// Fecha o gzip writer (flush + trailer)
	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("closing gzip writer: %w", err)
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

// addToTar adiciona um arquivo ou diretório ao tar archive.
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

	header, err := tar.FileInfoHeader(entry.Info, link)
	if err != nil {
		return fmt.Errorf("creating tar header for %s: %w", entry.Path, err)
	}

	// Usa o caminho relativo para preservar a estrutura
	header.Name = entry.RelPath

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", entry.Path, err)
	}

	// Se for arquivo regular, copia o conteúdo
	if entry.Info.Mode().IsRegular() {
		f, err := os.Open(entry.Path)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", entry.Path, err)
		}
		defer f.Close()

		if _, err := io.Copy(tw, f); err != nil {
			return fmt.Errorf("writing file %s to tar: %w", entry.Path, err)
		}
	}

	return nil
}

// countWriter conta os bytes escritos.
type countWriter struct {
	w io.Writer
	n uint64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += uint64(n)
	return n, err
}

// hashReader é um wrapper para io.Reader que calcula hash inline.
// Não utilizado diretamente na pipeline atual, mas disponível para o server.
var _ hash.Hash = sha256.New()
