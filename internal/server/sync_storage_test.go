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
	"sync"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
)

// newTestHandler cria um Handler minimal para testes de sync.
func newTestHandler(t *testing.T, storages map[string]config.StorageInfo) *Handler {
	t.Helper()
	cfg := &config.ServerConfig{
		Server:   config.ServerListen{Listen: ":0"},
		Storages: storages,
	}
	return NewHandler(cfg, slog.Default(), &sync.Map{}, &sync.Map{})
}

// createTestBackups cria arquivos de backup fake no diretório especificado.
func createTestBackups(t *testing.T, baseDir string, paths []string) {
	t.Helper()
	for _, p := range paths {
		fullPath := filepath.Join(baseDir, p)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("creating dir for %s: %v", p, err)
		}
		if err := os.WriteFile(fullPath, []byte("fake-backup-data"), 0644); err != nil {
			t.Fatalf("creating file %s: %v", p, err)
		}
	}
}

func TestSyncStorage_UploadsMissingFiles(t *testing.T) {
	baseDir := t.TempDir()

	// Cria 3 backups locais
	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
		"agent1/daily/2026-03-02T00-00-00-000.tar.gz",
		"agent1/daily/2026-03-03T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	// Mock: apenas 1 já existe no remoto
	mock := objstore.NewMockBackend()
	mock.Seed("backups/agent1/daily/2026-03-01T00-00-00-000.tar.gz")

	// Injeta o mock via monkey-patch do defaultBackendFactory
	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Total.Uploaded != 2 {
		t.Errorf("expected 2 uploads, got %d", result.Total.Uploaded)
	}
	if result.Total.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", result.Total.Skipped)
	}
	if result.Total.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", result.Total.Errors)
	}
	if len(result.Buckets) != 1 {
		t.Fatalf("expected 1 bucket result, got %d", len(result.Buckets))
	}
	if result.Buckets[0].StorageName != "primary" {
		t.Errorf("expected storage name 'primary', got %q", result.Buckets[0].StorageName)
	}
}

func TestSyncStorage_SkipsExisting(t *testing.T) {
	baseDir := t.TempDir()

	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
		"agent1/daily/2026-03-02T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	// Mock: todos já existem no remoto
	mock := objstore.NewMockBackend()
	mock.Seed(
		"backups/agent1/daily/2026-03-01T00-00-00-000.tar.gz",
		"backups/agent1/daily/2026-03-02T00-00-00-000.tar.gz",
	)

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result.Total.Uploaded != 0 {
		t.Errorf("expected 0 uploads (all exist), got %d", result.Total.Uploaded)
	}
	if result.Total.Skipped != 2 {
		t.Errorf("expected 2 skipped, got %d", result.Total.Skipped)
	}
	if len(mock.UploadCalls) != 0 {
		t.Errorf("expected 0 upload calls, got %d", len(mock.UploadCalls))
	}
}

func TestSyncStorage_OnlySyncMode(t *testing.T) {
	baseDir := t.TempDir()

	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	mockSync := objstore.NewMockBackend()
	mockOffload := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(cfg config.BucketConfig) (objstore.Backend, error) {
		if cfg.Name == "s3-sync" {
			return mockSync, nil
		}
		return mockOffload, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{
				{
					Name:   "s3-offload",
					Bucket: "offload-bucket",
					Prefix: "offload/",
					Mode:   config.BucketModeOffload,
					Retain: 5,
				},
				{
					Name:   "s3-sync",
					Bucket: "sync-bucket",
					Prefix: "sync/",
					Mode:   config.BucketModeSync,
				},
			},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	// Deve ter processado apenas o bucket sync
	if result.Total.Uploaded != 1 {
		t.Errorf("expected 1 upload (from sync bucket), got %d", result.Total.Uploaded)
	}
	if len(result.Buckets) != 1 {
		t.Fatalf("expected 1 bucket result (only sync), got %d", len(result.Buckets))
	}
	if result.Buckets[0].BucketName != "s3-sync" {
		t.Errorf("expected bucket 's3-sync', got %q", result.Buckets[0].BucketName)
	}

	// Mock offload não deve ter sido chamado
	if len(mockOffload.UploadCalls) != 0 {
		t.Errorf("offload mock should not have been called, got %d calls", len(mockOffload.UploadCalls))
	}
}

func TestSyncStorage_NoBackups(t *testing.T) {
	baseDir := t.TempDir()

	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result.Total.Uploaded != 0 {
		t.Errorf("expected 0 uploads, got %d", result.Total.Uploaded)
	}
	if result.Total.Skipped != 0 {
		t.Errorf("expected 0 skipped, got %d", result.Total.Skipped)
	}
}

func TestSyncStorage_GuardConcurrent(t *testing.T) {
	baseDir := t.TempDir()

	// Cria um backup local para que haja trabalho a fazer
	createTestBackups(t, baseDir, []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
	})

	// Mock com delay para simular upload lento
	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)

	// Simula sync running
	h.syncRunning.Store(true)

	// Segunda chamada deve retornar nil (guard ativo)
	result := h.SyncExistingStorage(context.Background())
	if result != nil {
		t.Errorf("expected nil result when sync already running, got %+v", result)
	}

	// Libera e tenta novamente
	h.syncRunning.Store(false)
	result = h.SyncExistingStorage(context.Background())
	if result == nil {
		t.Error("expected non-nil result after guard released")
	}
}

func TestSyncStorage_RespectsContextCancel(t *testing.T) {
	baseDir := t.TempDir()

	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
		"agent1/daily/2026-03-02T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)

	// Cancela ctx imediatamente
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := h.SyncExistingStorage(ctx)
	// Deve retornar rápido, sem uploads
	if result == nil {
		t.Fatal("expected non-nil result even with cancelled context")
	}
	if result.Total.Uploaded != 0 {
		t.Errorf("expected 0 uploads with cancelled context, got %d", result.Total.Uploaded)
	}
}

func TestSyncStorage_NoBuckets(t *testing.T) {
	baseDir := t.TempDir()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			// Sem buckets configurados
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Buckets) != 0 {
		t.Errorf("expected 0 bucket results, got %d", len(result.Buckets))
	}
}

func TestSyncStorage_UploadError(t *testing.T) {
	baseDir := t.TempDir()

	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	mock := objstore.NewMockBackend()
	mock.UploadErr = fmt.Errorf("simulated S3 failure")

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result.Total.Errors == 0 {
		t.Error("expected errors > 0 with upload failure")
	}
	if result.Total.Uploaded != 0 {
		t.Errorf("expected 0 uploads with failure, got %d", result.Total.Uploaded)
	}
}

func TestSyncStorage_ResultStored(t *testing.T) {
	baseDir := t.TempDir()

	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
	}
	createTestBackups(t, baseDir, localBackups)

	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	_ = h.SyncExistingStorage(context.Background())

	// Verifica que o resultado foi armazenado no lastSyncResult
	stored := h.lastSyncResult.Load()
	if stored == nil {
		t.Fatal("expected lastSyncResult to be stored")
	}
	storedResult, ok := stored.(*SyncStorageResult)
	if !ok {
		t.Fatal("lastSyncResult is not *SyncStorageResult")
	}
	if storedResult.Total.Uploaded != 1 {
		t.Errorf("expected stored result with 1 upload, got %d", storedResult.Total.Uploaded)
	}
	if storedResult.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestSyncStorage_ZstBackups(t *testing.T) {
	baseDir := t.TempDir()

	// Testa com backups .tar.zst
	localBackups := []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.zst",
		"agent1/daily/2026-03-02T00-00-00-000.tar.zst",
	}
	createTestBackups(t, baseDir, localBackups)

	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"primary": {
			BaseDir:    baseDir,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-mirror",
				Bucket: "test-bucket",
				Prefix: "backups/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result.Total.Uploaded != 2 {
		t.Errorf("expected 2 uploads for .tar.zst files, got %d", result.Total.Uploaded)
	}
}

func TestSyncStorage_MultipleStorages(t *testing.T) {
	baseDir1 := t.TempDir()
	baseDir2 := t.TempDir()

	createTestBackups(t, baseDir1, []string{
		"agent1/daily/2026-03-01T00-00-00-000.tar.gz",
	})
	createTestBackups(t, baseDir2, []string{
		"agent2/daily/2026-03-01T00-00-00-000.tar.gz",
		"agent2/daily/2026-03-02T00-00-00-000.tar.gz",
	})

	mock := objstore.NewMockBackend()

	origFactory := defaultBackendFactory
	defaultBackendFactory = func(_ config.BucketConfig) (objstore.Backend, error) {
		return mock, nil
	}
	defer func() { defaultBackendFactory = origFactory }()

	storages := map[string]config.StorageInfo{
		"storage-a": {
			BaseDir:    baseDir1,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-a",
				Bucket: "bucket-a",
				Prefix: "a/",
				Mode:   config.BucketModeSync,
			}},
		},
		"storage-b": {
			BaseDir:    baseDir2,
			MaxBackups: 10,
			Buckets: []config.BucketConfig{{
				Name:   "s3-b",
				Bucket: "bucket-b",
				Prefix: "b/",
				Mode:   config.BucketModeSync,
			}},
		},
	}

	h := newTestHandler(t, storages)
	result := h.SyncExistingStorage(context.Background())

	if result.Total.Uploaded != 3 {
		t.Errorf("expected 3 total uploads across storages, got %d", result.Total.Uploaded)
	}
	if len(result.Buckets) != 2 {
		t.Errorf("expected 2 bucket results, got %d", len(result.Buckets))
	}
}

func TestListLocalBackups(t *testing.T) {
	baseDir := t.TempDir()

	// Cria mistura de arquivos válidos e inválidos
	files := []string{
		"agent1/daily/2026-03-01.tar.gz",
		"agent1/daily/2026-03-02.tar.zst",
		"agent1/daily/notes.txt", // não é backup
		"agent1/daily/.tmp",      // não é backup
		"agent2/weekly/backup.tar.gz",
	}
	createTestBackups(t, baseDir, files)
	// Cria diretório chunks_ que deve ser skip
	os.MkdirAll(filepath.Join(baseDir, "chunks_abc", "00", "00"), 0755)
	os.WriteFile(filepath.Join(baseDir, "chunks_abc", "00", "00", "chunk.dat"), []byte("chunk"), 0644)

	result, err := listLocalBackups(baseDir)
	if err != nil {
		t.Fatalf("listLocalBackups failed: %v", err)
	}

	// Deve encontrar apenas 3 backups (ignora notes.txt, .tmp, e chunks_)
	if len(result) != 3 {
		t.Errorf("expected 3 backups, got %d", len(result))
		for _, f := range result {
			t.Logf("  found: %s", f.RelPath)
		}
	}
}

func TestUploadWithRetryStandalone(t *testing.T) {
	t.Run("success_first_attempt", func(t *testing.T) {
		mock := objstore.NewMockBackend()
		err := uploadWithRetryStandalone(context.Background(), mock, "/dev/null", "test/file.tar.gz", 3, 30*time.Minute, slog.Default())
		if err != nil {
			t.Errorf("expected success, got: %v", err)
		}
	})

	t.Run("all_attempts_fail", func(t *testing.T) {
		mock := objstore.NewMockBackend()
		mock.UploadErr = fmt.Errorf("permanent failure")
		err := uploadWithRetryStandalone(context.Background(), mock, "/dev/null", "test/file.tar.gz", 2, 1*time.Second, slog.Default())
		if err == nil {
			t.Error("expected error after all retries, got nil")
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		mock := objstore.NewMockBackend()
		mock.UploadErr = fmt.Errorf("transient failure")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := uploadWithRetryStandalone(ctx, mock, "/dev/null", "test/file.tar.gz", 3, 30*time.Minute, slog.Default())
		if err == nil {
			t.Error("expected error with cancelled context, got nil")
		}
	})
}
