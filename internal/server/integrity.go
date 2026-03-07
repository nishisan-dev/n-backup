// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// integrity.go contém a lógica de verificação de integridade de arquivos
// de backup comprimidos (.tar.gz / .tar.zst).
//
// VerifyArchiveIntegrity lê e descomprime o tarball inteiro, validando
// a estrutura tar e a integridade da compressão end-to-end.
// Equivalente funcional de: tar -I zstd -tf - > /dev/null

package server

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// countingReader wraps an io.Reader and atomically tracks bytes read.
// Used by VerifyArchiveIntegrity to expose progress to the WebUI.
type countingReader struct {
	reader   io.Reader
	progress *IntegrityProgress
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	if n > 0 && cr.progress != nil {
		cr.progress.BytesRead.Add(int64(n))
	}
	return n, err
}

// VerifyArchiveIntegrity valida a integridade de um arquivo tar comprimido.
// Detecta o tipo de compressão pela extensão do arquivo (.tar.gz ou .tar.zst),
// descomprime e itera todos os entries do tar, drenando seus conteúdos.
// Retorna nil se o archive é válido, ou um erro descritivo caso contrário.
//
// Quando progress != nil, atualiza atomicamente BytesRead e Entries para
// permitir observação do progresso em tempo real pelo WebUI.
// Quando logger != nil, emite logs periódicos de progresso a cada ~30s.
//
// Esta verificação é equivalente a:
//
//	tar -tzf arquivo.tar.gz > /dev/null     (gzip)
//	tar -I zstd -tf arquivo.tar.zst > /dev/null  (zstd)
func VerifyArchiveIntegrity(path string, progress *IntegrityProgress, logger *slog.Logger) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening archive for integrity check: %w", err)
	}
	defer f.Close()

	// Verifica que o arquivo não está vazio
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stating archive: %w", err)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("archive is empty (0 bytes)")
	}

	// Inicializa progresso se fornecido
	if progress != nil {
		progress.TotalBytes.Store(fi.Size())
		progress.StartedAt.Store(time.Now())
	}

	// Wrap o file reader com counting quando há progresso
	var fileReader io.Reader = f
	if progress != nil {
		fileReader = &countingReader{reader: f, progress: progress}
	}

	var decompReader io.Reader

	switch {
	case strings.HasSuffix(path, ".tar.gz"):
		gz, err := gzip.NewReader(fileReader)
		if err != nil {
			return fmt.Errorf("initializing gzip reader: %w", err)
		}
		defer gz.Close()
		decompReader = gz

	case strings.HasSuffix(path, ".tar.zst"):
		zr, err := zstd.NewReader(fileReader)
		if err != nil {
			return fmt.Errorf("initializing zstd reader: %w", err)
		}
		defer zr.Close()
		decompReader = zr

	default:
		return fmt.Errorf("unsupported archive extension: %s", path)
	}

	tr := tar.NewReader(decompReader)
	entryCount := 0
	startTime := time.Now()
	lastLogTime := startTime

	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry #%d: %w", entryCount+1, err)
		}

		// Drena o conteúdo do entry para validar os dados comprimidos
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("reading content of tar entry #%d: %w", entryCount+1, err)
		}

		entryCount++

		// Atualiza progresso atômico
		if progress != nil {
			progress.Entries.Store(int64(entryCount))
		}

		// Log periódico a cada ~30s
		if logger != nil && time.Since(lastLogTime) >= 30*time.Second {
			lastLogTime = time.Now()
			var bytesRead, totalBytes int64
			var pct float64
			if progress != nil {
				bytesRead = progress.BytesRead.Load()
				totalBytes = progress.TotalBytes.Load()
				if totalBytes > 0 {
					pct = float64(bytesRead) / float64(totalBytes) * 100
				}
			}
			logger.Info("integrity_progress",
				"bytes_read", formatBytesGo(bytesRead),
				"total", formatBytesGo(totalBytes),
				"entries", entryCount,
				"pct", fmt.Sprintf("%.1f%%", pct),
				"elapsed", time.Since(startTime).Truncate(time.Second).String(),
			)
		}
	}

	if entryCount == 0 {
		return fmt.Errorf("archive contains no entries")
	}

	// Log final
	if logger != nil {
		duration := time.Since(startTime)
		var bytesRead int64
		if progress != nil {
			bytesRead = progress.BytesRead.Load()
		}
		logger.Info("integrity_complete",
			"entries", entryCount,
			"bytes", formatBytesGo(bytesRead),
			"duration", duration.Truncate(time.Second).String(),
		)
	}

	return nil
}
