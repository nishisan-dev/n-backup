// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestScanner_BasicScan(t *testing.T) {
	dir := createTestTree(t)

	scanner := NewScanner([]string{dir}, nil)

	var files []string
	err := scanner.Scan(context.Background(), func(entry FileEntry) error {
		files = append(files, entry.RelPath)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Deve encontrar pelo menos o diretório raiz, file1.txt, file2.txt, sub/file3.txt
	if len(files) < 4 {
		t.Errorf("expected at least 4 entries, got %d: %v", len(files), files)
	}
}

func TestScanner_ExcludePattern(t *testing.T) {
	dir := createTestTree(t)

	scanner := NewScanner([]string{dir}, []string{"*.log"})

	var files []string
	err := scanner.Scan(context.Background(), func(entry FileEntry) error {
		files = append(files, filepath.Base(entry.Path))
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, f := range files {
		if f == "access.log" {
			t.Error("expected access.log to be excluded")
		}
	}
}

func TestScanner_ExcludeDirectory(t *testing.T) {
	dir := createTestTree(t)

	scanner := NewScanner([]string{dir}, []string{".git/**"})

	var files []string
	err := scanner.Scan(context.Background(), func(entry FileEntry) error {
		files = append(files, entry.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, f := range files {
		if filepath.Base(f) == "config" && filepath.Dir(f) != dir {
			// Verifica que não pegou .git/config
			rel, _ := filepath.Rel(dir, f)
			if rel == ".git/config" || rel == filepath.Join(".git", "config") {
				t.Error("expected .git directory to be excluded")
			}
		}
	}
}

func TestScanner_ContextCancellation(t *testing.T) {
	dir := createTestTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancela imediatamente

	scanner := NewScanner([]string{dir}, nil)
	err := scanner.Scan(ctx, func(entry FileEntry) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestStream_ProducesValidTarGz(t *testing.T) {
	dir := createTestTree(t)

	scanner := NewScanner([]string{dir}, []string{"*.log", ".git/**"})

	var buf bytes.Buffer
	result, err := Stream(context.Background(), scanner, &buf, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if result.Size == 0 {
		t.Error("expected non-zero stream size")
	}

	// Verifica que é um gzip válido
	gzReader, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("invalid gzip: %v", err)
	}
	defer gzReader.Close()

	// Verifica que é um tar válido
	tr := tar.NewReader(gzReader)
	var tarFiles []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		tarFiles = append(tarFiles, hdr.Name)
	}

	if len(tarFiles) == 0 {
		t.Error("expected at least one file in tar archive")
	}

	// Verifica que excludes foram respeitados
	for _, f := range tarFiles {
		if filepath.Base(f) == "access.log" {
			t.Error("access.log should be excluded from tar")
		}
	}
}

func TestStream_ChecksumConsistency(t *testing.T) {
	dir := createTestTree(t)

	scanner := NewScanner([]string{dir}, nil)

	var buf bytes.Buffer
	result, err := Stream(context.Background(), scanner, &buf, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// O checksum não deve ser zero
	var zero [32]byte
	if result.Checksum == zero {
		t.Error("expected non-zero checksum")
	}

	// Size deve bater com o buffer
	if result.Size != uint64(buf.Len()) {
		t.Errorf("size mismatch: result=%d buffer=%d", result.Size, buf.Len())
	}
}

// createTestTree cria uma estrutura de arquivos temporária para teste.
func createTestTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Cria arquivos
	writeFile(t, filepath.Join(dir, "file1.txt"), "content of file 1")
	writeFile(t, filepath.Join(dir, "file2.txt"), "content of file 2")
	writeFile(t, filepath.Join(dir, "access.log"), "log content")

	// Cria subdiretório
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	writeFile(t, filepath.Join(sub, "file3.txt"), "content of file 3")

	// Cria .git (para testar exclude)
	gitDir := filepath.Join(dir, ".git")
	os.MkdirAll(gitDir, 0755)
	writeFile(t, filepath.Join(gitDir, "config"), "git config")

	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing file %s: %v", path, err)
	}
}
