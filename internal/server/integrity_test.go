// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// createTestTarGz cria um arquivo .tar.gz válido com uma entrada de teste.
func createTestTarGz(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating file %s: %v", path, err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Escreve 2 entries para ter um archive realista
	for _, entry := range []struct {
		name    string
		content string
	}{
		{"hello.txt", "Hello, world!\n"},
		{"data/config.yaml", "key: value\nlist:\n  - item1\n  - item2\n"},
	} {
		hdr := &tar.Header{
			Name: entry.name,
			Mode: 0644,
			Size: int64(len(entry.content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(entry.content)); err != nil {
			t.Fatalf("writing tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	return path
}

// createTestTarZst cria um arquivo .tar.zst válido com uma entrada de teste.
func createTestTarZst(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating file %s: %v", path, err)
	}
	defer f.Close()

	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatalf("creating zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)

	for _, entry := range []struct {
		name    string
		content string
	}{
		{"backup/db.sql", "CREATE TABLE users (id INT);\n"},
		{"backup/metadata.json", `{"version": 1, "timestamp": "2026-03-03T18:00:00Z"}`},
	} {
		hdr := &tar.Header{
			Name: entry.name,
			Mode: 0644,
			Size: int64(len(entry.content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write([]byte(entry.content)); err != nil {
			t.Fatalf("writing tar content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zstd writer: %v", err)
	}

	return path
}

func TestVerifyArchiveIntegrity_ValidTarGz(t *testing.T) {
	dir := t.TempDir()
	path := createTestTarGz(t, dir, "valid.tar.gz")

	if err := VerifyArchiveIntegrity(path); err != nil {
		t.Fatalf("expected valid tar.gz to pass, got: %v", err)
	}
}

func TestVerifyArchiveIntegrity_ValidTarZst(t *testing.T) {
	dir := t.TempDir()
	path := createTestTarZst(t, dir, "valid.tar.zst")

	if err := VerifyArchiveIntegrity(path); err != nil {
		t.Fatalf("expected valid tar.zst to pass, got: %v", err)
	}
}

func TestVerifyArchiveIntegrity_CorruptTarGz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.tar.gz")
	os.WriteFile(path, []byte("this is not a gzip file at all"), 0644)

	if err := VerifyArchiveIntegrity(path); err == nil {
		t.Fatal("expected corrupt tar.gz to fail integrity check")
	}
}

func TestVerifyArchiveIntegrity_CorruptTarZst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.tar.zst")
	os.WriteFile(path, []byte("this is not a zstd file at all"), 0644)

	if err := VerifyArchiveIntegrity(path); err == nil {
		t.Fatal("expected corrupt tar.zst to fail integrity check")
	}
}

func TestVerifyArchiveIntegrity_TruncatedTarGz(t *testing.T) {
	dir := t.TempDir()
	path := createTestTarGz(t, dir, "truncated.tar.gz")

	// Trunca o arquivo pela metade
	fi, _ := os.Stat(path)
	os.Truncate(path, fi.Size()/2)

	if err := VerifyArchiveIntegrity(path); err == nil {
		t.Fatal("expected truncated tar.gz to fail integrity check")
	}
}

func TestVerifyArchiveIntegrity_TruncatedTarZst(t *testing.T) {
	dir := t.TempDir()
	path := createTestTarZst(t, dir, "truncated.tar.zst")

	fi, _ := os.Stat(path)
	os.Truncate(path, fi.Size()/2)

	if err := VerifyArchiveIntegrity(path); err == nil {
		t.Fatal("expected truncated tar.zst to fail integrity check")
	}
}

func TestVerifyArchiveIntegrity_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.tar.gz")
	os.WriteFile(path, []byte{}, 0644)

	if err := VerifyArchiveIntegrity(path); err == nil {
		t.Fatal("expected empty file to fail integrity check")
	}
}

func TestVerifyArchiveIntegrity_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.tar.bz2")
	os.WriteFile(path, []byte("data"), 0644)

	err := VerifyArchiveIntegrity(path)
	if err == nil {
		t.Fatal("expected unsupported extension to fail")
	}
}

func TestVerifyArchiveIntegrity_NonExistentFile(t *testing.T) {
	err := VerifyArchiveIntegrity("/nonexistent/path/backup.tar.gz")
	if err == nil {
		t.Fatal("expected non-existent file to fail")
	}
}
