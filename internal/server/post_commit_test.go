// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
)

func newTestOrchestrator(t *testing.T, buckets []config.BucketConfig, backends map[string]*objstore.MockBackend) *PostCommitOrchestrator {
	t.Helper()
	factory := func(cfg config.BucketConfig) (objstore.Backend, error) {
		b, ok := backends[cfg.Name]
		if !ok {
			return nil, fmt.Errorf("no mock backend for %q", cfg.Name)
		}
		return b, nil
	}
	o, err := NewPostCommitOrchestrator(buckets, factory, slog.Default())
	if err != nil {
		t.Fatalf("NewPostCommitOrchestrator: %v", err)
	}
	return o
}

func TestPostCommit_SyncMode(t *testing.T) {
	mock := objstore.NewMockBackend()
	// Simula backups antigos já no bucket
	mock.Seed("scripts/old-backup-1.tar.gz", "scripts/old-backup-2.tar.gz")

	buckets := []config.BucketConfig{{
		Name:     "s3-mirror",
		Provider: "s3",
		Bucket:   "test-bucket",
		Prefix:   "scripts/",
		Mode:     config.BucketModeSync,
	}}

	backends := map[string]*objstore.MockBackend{"s3-mirror": mock}
	o := newTestOrchestrator(t, buckets, backends)

	// Cria arquivo local de backup para upload
	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "2026-01-01T00-00-00-000.tar.gz")
	if err := os.WriteFile(backupFile, []byte("backup data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simula que o Rotate removeu "old-backup-2.tar.gz"
	rotated := []string{"old-backup-2.tar.gz"}

	results := o.Execute(context.Background(), backupFile, rotated, tmpDir)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("sync should succeed, got error: %v", results[0].Error)
	}

	// Deve ter feito upload do novo backup
	if len(mock.UploadCalls) != 1 || mock.UploadCalls[0] != "scripts/2026-01-01T00-00-00-000.tar.gz" {
		t.Errorf("expected upload of new backup, got %v", mock.UploadCalls)
	}

	// Deve ter deletado o backup rotated do bucket
	if len(mock.DeleteCalls) != 1 || mock.DeleteCalls[0] != "scripts/old-backup-2.tar.gz" {
		t.Errorf("expected delete of rotated backup, got %v", mock.DeleteCalls)
	}

	// Arquivo local deve continuar existindo
	if _, err := os.Stat(backupFile); err != nil {
		t.Errorf("local file should still exist: %v", err)
	}
}

func TestPostCommit_OffloadMode(t *testing.T) {
	mock := objstore.NewMockBackend()

	buckets := []config.BucketConfig{{
		Name:     "offload-nas",
		Provider: "s3",
		Bucket:   "primary-backups",
		Prefix:   "scripts/",
		Mode:     config.BucketModeOffload,
		Retain:   3,
	}}

	backends := map[string]*objstore.MockBackend{"offload-nas": mock}
	o := newTestOrchestrator(t, buckets, backends)

	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "2026-01-01T00-00-00-000.tar.gz")
	if err := os.WriteFile(backupFile, []byte("backup data"), 0644); err != nil {
		t.Fatal(err)
	}

	results := o.Execute(context.Background(), backupFile, nil, tmpDir)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("offload should succeed, got error: %v", results[0].Error)
	}

	// Upload deve ter sido feito
	if len(mock.UploadCalls) != 1 {
		t.Fatalf("expected 1 upload call, got %d", len(mock.UploadCalls))
	}

	// Arquivo local deve ter sido removido
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Error("local file should have been removed by offload")
	}
}

func TestPostCommit_OffloadMode_UploadFailPreservesLocal(t *testing.T) {
	mock := objstore.NewMockBackend()
	mock.UploadErr = fmt.Errorf("simulated upload failure")

	buckets := []config.BucketConfig{{
		Name:     "offload-fail",
		Provider: "s3",
		Bucket:   "primary-backups",
		Prefix:   "scripts/",
		Mode:     config.BucketModeOffload,
		Retain:   3,
	}}

	backends := map[string]*objstore.MockBackend{"offload-fail": mock}
	o := newTestOrchestrator(t, buckets, backends)
	o.maxRetry = 1 // acelera test

	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "2026-01-01T00-00-00-000.tar.gz")
	if err := os.WriteFile(backupFile, []byte("backup data"), 0644); err != nil {
		t.Fatal(err)
	}

	results := o.Execute(context.Background(), backupFile, nil, tmpDir)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Success {
		t.Fatal("offload with upload error should fail")
	}

	// Arquivo local DEVE sobreviver quando upload falha
	if _, err := os.Stat(backupFile); err != nil {
		t.Errorf("local file must survive when upload fails: %v", err)
	}
}

func TestPostCommit_ArchiveMode(t *testing.T) {
	mock := objstore.NewMockBackend()

	buckets := []config.BucketConfig{{
		Name:     "cold-archive",
		Provider: "s3",
		Bucket:   "cold-backups",
		Prefix:   "scripts/",
		Mode:     config.BucketModeArchive,
		Retain:   5,
	}}

	backends := map[string]*objstore.MockBackend{"cold-archive": mock}
	o := newTestOrchestrator(t, buckets, backends)

	tmpDir := t.TempDir()

	// Cria arquivos que seriam rotacionados
	for _, name := range []string{"old-1.tar.gz", "old-2.tar.gz"} {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte("old backup"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	rotated := []string{"old-1.tar.gz", "old-2.tar.gz"}
	results := o.Execute(context.Background(), "/fake/new-backup.tar.gz", rotated, tmpDir)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("archive should succeed, got error: %v", results[0].Error)
	}

	// Upload dos 2 arquivos rotated
	if len(mock.UploadCalls) != 2 {
		t.Fatalf("expected 2 upload calls, got %d: %v", len(mock.UploadCalls), mock.UploadCalls)
	}
}

func TestPostCommit_OffloadRetainRotatesBucket(t *testing.T) {
	mock := objstore.NewMockBackend()
	// Pré-popula com 4 backups no bucket, retain=3
	mock.Seed(
		"scripts/2026-01-01.tar.gz",
		"scripts/2026-01-02.tar.gz",
		"scripts/2026-01-03.tar.gz",
		"scripts/2026-01-04.tar.gz",
	)

	buckets := []config.BucketConfig{{
		Name:     "offload-rotate",
		Provider: "s3",
		Bucket:   "test-bucket",
		Prefix:   "scripts/",
		Mode:     config.BucketModeOffload,
		Retain:   3,
	}}

	backends := map[string]*objstore.MockBackend{"offload-rotate": mock}
	o := newTestOrchestrator(t, buckets, backends)

	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "2026-01-05.tar.gz")
	if err := os.WriteFile(backupFile, []byte("new backup"), 0644); err != nil {
		t.Fatal(err)
	}

	results := o.Execute(context.Background(), backupFile, nil, tmpDir)
	if !results[0].Success {
		t.Fatalf("offload should succeed: %v", results[0].Error)
	}

	// Bucket agora tem 5 objetos, retain=3, deve ter removido os 2 mais antigos
	keys := mock.ObjectKeys()
	if len(keys) != 3 {
		t.Fatalf("expected 3 objects after rotation, got %d: %v", len(keys), keys)
	}
}

func TestPostCommit_MultipleBucketsParallel(t *testing.T) {
	syncMock := objstore.NewMockBackend()
	archiveMock := objstore.NewMockBackend()

	buckets := []config.BucketConfig{
		{Name: "sync-main", Provider: "s3", Bucket: "b1", Prefix: "p/", Mode: config.BucketModeSync},
		{Name: "archive-cold", Provider: "s3", Bucket: "b2", Prefix: "p/", Mode: config.BucketModeArchive, Retain: 10},
	}

	backends := map[string]*objstore.MockBackend{
		"sync-main":    syncMock,
		"archive-cold": archiveMock,
	}
	o := newTestOrchestrator(t, buckets, backends)

	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "backup.tar.gz")
	if err := os.WriteFile(backupFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	rotated := []string{"old.tar.gz"}
	// Cria o rotated file para archive
	os.WriteFile(filepath.Join(tmpDir, "old.tar.gz"), []byte("old"), 0644)

	results := o.Execute(context.Background(), backupFile, rotated, tmpDir)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	for _, r := range results {
		if !r.Success {
			t.Errorf("bucket %q failed: %v", r.BucketName, r.Error)
		}
	}

	// sync: 1 upload + 0 deletes (old.tar.gz pode não estar no bucket)
	if len(syncMock.UploadCalls) != 1 {
		t.Errorf("sync: expected 1 upload, got %d", len(syncMock.UploadCalls))
	}

	// archive: 1 upload (rotated file)
	if len(archiveMock.UploadCalls) != 1 {
		t.Errorf("archive: expected 1 upload, got %d", len(archiveMock.UploadCalls))
	}
}

func TestPostCommit_Nil(t *testing.T) {
	var o *PostCommitOrchestrator
	results := o.Execute(context.Background(), "/fake/path", nil, "/fake")
	if results != nil {
		t.Errorf("nil orchestrator should return nil results, got %v", results)
	}
	if o.HasBlockingBuckets() {
		t.Error("nil orchestrator should not have blocking buckets")
	}
}

func TestPostCommit_NoBuckets(t *testing.T) {
	o, err := NewPostCommitOrchestrator(nil, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if o != nil {
		t.Error("expected nil orchestrator for empty buckets")
	}
}
