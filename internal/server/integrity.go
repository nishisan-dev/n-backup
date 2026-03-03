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
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// VerifyArchiveIntegrity valida a integridade de um arquivo tar comprimido.
// Detecta o tipo de compressão pela extensão do arquivo (.tar.gz ou .tar.zst),
// descomprime e itera todos os entries do tar, drenando seus conteúdos.
// Retorna nil se o archive é válido, ou um erro descritivo caso contrário.
//
// Esta verificação é equivalente a:
//
//	tar -tzf arquivo.tar.gz > /dev/null     (gzip)
//	tar -I zstd -tf arquivo.tar.zst > /dev/null  (zstd)
func VerifyArchiveIntegrity(path string) error {
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

	var decompReader io.Reader

	switch {
	case strings.HasSuffix(path, ".tar.gz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("initializing gzip reader: %w", err)
		}
		defer gz.Close()
		decompReader = gz

	case strings.HasSuffix(path, ".tar.zst"):
		zr, err := zstd.NewReader(f)
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
	}

	if entryCount == 0 {
		return fmt.Errorf("archive contains no entries")
	}

	return nil
}
