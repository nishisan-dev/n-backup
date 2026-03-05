// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// sync_storage.go contém a lógica de sincronização retroativa de backups
// locais com Object Storage. Quando o operador adiciona configuração de
// buckets (mode: sync) a um storage que já possui backups locais, este
// módulo faz upload dos artefatos faltantes no bucket remoto.
//
// Acionado via SIGUSR1 no daemon. O CLI envia o sinal através do
// subcomando "nbackup-server sync-storage".
//
// Apenas buckets no modo "sync" são processados — offload e archive
// possuem semântica destrutiva que não faz sentido retroativamente.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
)

// ---------------------------------------------------------------------------
// Tipos de resultado — expostos para observabilidade e WebUI
// ---------------------------------------------------------------------------

// SyncStorageResult agrega o resultado de uma sincronização completa
// (todos os storages/buckets). Exposto para permitir exibição no WebUI.
type SyncStorageResult struct {
	StartedAt time.Time          `json:"started_at"`
	EndedAt   time.Time          `json:"ended_at"`
	Duration  time.Duration      `json:"duration"`
	Buckets   []SyncBucketResult `json:"buckets"`
	Total     SyncStorageTotals  `json:"total"`
}

// SyncStorageTotals resume contadores agregados de toda a operação.
type SyncStorageTotals struct {
	Uploaded int `json:"uploaded"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
}

// SyncBucketResult reporta o resultado da sincronização de um bucket específico.
type SyncBucketResult struct {
	StorageName string        `json:"storage_name"`
	BucketName  string        `json:"bucket_name"`
	Mode        string        `json:"mode"`
	Uploaded    int           `json:"uploaded"`
	Skipped     int           `json:"skipped"`
	Errors      int           `json:"errors"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Progresso em tempo real — leitura lock-free pelo WebUI
// ---------------------------------------------------------------------------

// SyncProgress rastreia o progresso em tempo real de uma sincronização ativa.
// Campos atômicos para leitura lock-free pelo endpoint HTTP.
type SyncProgress struct {
	Running        atomic.Bool
	StartedAt      atomic.Value // time.Time
	CurrentFile    atomic.Value // string — path relativo do arquivo sendo enviado
	CurrentBucket  atomic.Value // string — nome do bucket atual
	TotalFiles     atomic.Int64 // total de arquivos a processar (todos os buckets)
	ProcessedFiles atomic.Int64 // arquivos já processados (uploaded + skipped + errors)
	UploadedFiles  atomic.Int64
	SkippedFiles   atomic.Int64
	ErrorFiles     atomic.Int64
	BytesUploaded  atomic.Int64 // bytes total enviados (soma de file sizes uploaded)
}

// ---------------------------------------------------------------------------
// Entry point — chamado pelo signal handler
// ---------------------------------------------------------------------------

// SyncExistingStorage percorre todos os storages configurados que possuem
// buckets no modo sync, e faz upload dos backups locais que não existem
// no bucket remoto. Thread-safe: usa syncRunning como guard atômico para
// evitar execuções concorrentes.
//
// Retorna o resultado agregado para observabilidade.
func (h *Handler) SyncExistingStorage(ctx context.Context) *SyncStorageResult {
	// Guard: apenas uma execução por vez
	if !h.syncRunning.CompareAndSwap(false, true) {
		h.logger.Warn("sync-storage: already running, skipping duplicate signal")
		if h.Events != nil {
			h.Events.PushEvent("warn", "sync_storage_skipped", "",
				"sync-storage signal received but sync is already running", 0)
		}
		return nil
	}
	defer h.syncRunning.Store(false)

	now := time.Now()
	result := &SyncStorageResult{
		StartedAt: now,
	}

	// Inicializa progresso em tempo real
	p := &h.syncProgress
	p.Running.Store(true)
	p.StartedAt.Store(now)
	p.CurrentFile.Store("")
	p.CurrentBucket.Store("")
	p.TotalFiles.Store(0)
	p.ProcessedFiles.Store(0)
	p.UploadedFiles.Store(0)
	p.SkippedFiles.Store(0)
	p.ErrorFiles.Store(0)
	p.BytesUploaded.Store(0)
	defer p.Running.Store(false)

	h.logger.Info("sync-storage: starting retroactive storage sync")
	if h.Events != nil {
		h.Events.PushEvent("info", "sync_storage_started", "",
			"retroactive storage sync initiated via signal", 0)
	}

	// Itera storages de forma determinística (ordenado por nome)
	storageNames := make([]string, 0, len(h.cfg.Storages))
	for name := range h.cfg.Storages {
		storageNames = append(storageNames, name)
	}
	sort.Strings(storageNames)

	// Pré-scan: conta o total de arquivos locais a processar para progresso global
	for _, storageName := range storageNames {
		si := h.cfg.Storages[storageName]
		syncBuckets := filterBucketsByMode(si.Buckets, config.BucketModeSync)
		if len(syncBuckets) == 0 {
			continue
		}
		localFiles, err := listLocalBackups(si.BaseDir)
		if err == nil {
			p.TotalFiles.Add(int64(len(localFiles)) * int64(len(syncBuckets)))
		}
	}

	for _, storageName := range storageNames {
		si := h.cfg.Storages[storageName]

		// Filtra apenas buckets sync
		syncBuckets := filterBucketsByMode(si.Buckets, config.BucketModeSync)
		if len(syncBuckets) == 0 {
			continue
		}

		for _, bcfg := range syncBuckets {
			// Respeita cancelamento
			select {
			case <-ctx.Done():
				h.logger.Info("sync-storage: cancelled by context")
				result.EndedAt = time.Now()
				result.Duration = result.EndedAt.Sub(result.StartedAt)
				return result
			default:
			}

			p.CurrentBucket.Store(bcfg.Name)
			logger := h.logger.With("storage", storageName, "bucket", bcfg.Name)
			br := h.syncOneBucket(ctx, storageName, si, bcfg, logger)
			result.Buckets = append(result.Buckets, br)
			result.Total.Uploaded += br.Uploaded
			result.Total.Skipped += br.Skipped
			result.Total.Errors += br.Errors
		}
	}

	result.EndedAt = time.Now()
	result.Duration = result.EndedAt.Sub(result.StartedAt)

	h.logger.Info("sync-storage: completed",
		"duration", result.Duration,
		"uploaded", result.Total.Uploaded,
		"skipped", result.Total.Skipped,
		"errors", result.Total.Errors,
	)

	if h.Events != nil {
		h.Events.PushEvent("info", "sync_storage_completed", "",
			fmt.Sprintf("sync completed in %s — uploaded: %d, skipped: %d, errors: %d",
				result.Duration.Round(time.Millisecond), result.Total.Uploaded, result.Total.Skipped, result.Total.Errors), 0)
	}

	// Armazena último resultado para consulta futura (WebUI)
	h.lastSyncResult.Store(result)

	return result
}

// ---------------------------------------------------------------------------
// Sync de um storage+bucket específico
// ---------------------------------------------------------------------------

// syncOneBucket sincroniza um storage+bucket específico.
// Lista backups locais, compara com remotos, e faz upload dos faltantes.
func (h *Handler) syncOneBucket(ctx context.Context, storageName string, si config.StorageInfo, bcfg config.BucketConfig, logger *slog.Logger) SyncBucketResult {
	start := time.Now()
	br := SyncBucketResult{
		StorageName: storageName,
		BucketName:  bcfg.Name,
		Mode:        bcfg.Mode,
	}

	// Instancia backend
	backend, err := defaultBackendFactory(bcfg)
	if err != nil {
		logger.Error("sync-storage: failed to create backend", "error", err)
		br.Errors++
		br.Error = fmt.Sprintf("backend creation failed: %v", err)
		br.Duration = time.Since(start)
		return br
	}

	// Lista backups locais (recursivo sob BaseDir)
	localFiles, err := listLocalBackups(si.BaseDir)
	if err != nil {
		logger.Error("sync-storage: failed to list local backups", "error", err, "base_dir", si.BaseDir)
		br.Errors++
		br.Error = fmt.Sprintf("local listing failed: %v", err)
		br.Duration = time.Since(start)
		return br
	}

	if len(localFiles) == 0 {
		logger.Info("sync-storage: no local backups found", "base_dir", si.BaseDir)
		br.Duration = time.Since(start)
		return br
	}

	// Lista objetos remotos
	remoteObjs, err := backend.List(ctx, bcfg.Prefix)
	if err != nil {
		logger.Error("sync-storage: failed to list remote objects", "error", err, "prefix", bcfg.Prefix)
		br.Errors++
		br.Error = fmt.Sprintf("remote listing failed: %v", err)
		br.Duration = time.Since(start)
		return br
	}

	// Build set de chaves remotas para lookup O(1)
	remoteKeySet := make(map[string]struct{}, len(remoteObjs))
	for _, obj := range remoteObjs {
		remoteKeySet[obj.Key] = struct{}{}
	}

	logger.Info("sync-storage: comparing local vs remote",
		"local_count", len(localFiles),
		"remote_count", len(remoteObjs),
	)

	// Upload dos faltantes
	p := &h.syncProgress
	for _, lf := range localFiles {
		select {
		case <-ctx.Done():
			logger.Info("sync-storage: cancelled by context during upload loop")
			br.Duration = time.Since(start)
			return br
		default:
		}

		p.CurrentFile.Store(lf.RelPath)

		// Constrói a chave remota: prefix + path relativo ao BaseDir
		remoteKey := bcfg.Prefix + lf.RelPath
		if _, exists := remoteKeySet[remoteKey]; exists {
			br.Skipped++
			p.SkippedFiles.Add(1)
			p.ProcessedFiles.Add(1)
			continue
		}

		// Upload com retry
		uploadLogger := logger.With("file", lf.RelPath, "remote_key", remoteKey)
		if err := uploadWithRetryStandalone(ctx, backend, lf.AbsPath, remoteKey, 3, 30*time.Minute, uploadLogger); err != nil {
			uploadLogger.Error("sync-storage: upload failed", "error", err)
			br.Errors++
			p.ErrorFiles.Add(1)
			p.ProcessedFiles.Add(1)
			if h.Events != nil {
				h.Events.PushEvent("error", "sync_storage_upload_failed", "",
					fmt.Sprintf("sync upload failed: %s → %s: %v", lf.RelPath, remoteKey, err), 0)
			}
		} else {
			br.Uploaded++
			p.UploadedFiles.Add(1)
			p.ProcessedFiles.Add(1)
			// Contabiliza bytes do arquivo uploaded
			if fi, statErr := os.Stat(lf.AbsPath); statErr == nil {
				p.BytesUploaded.Add(fi.Size())
			}
			uploadLogger.Info("sync-storage: uploaded successfully")
		}
	}

	br.Duration = time.Since(start)
	return br
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// localBackupFile representa um backup encontrado no filesystem local.
type localBackupFile struct {
	AbsPath string // caminho absoluto
	RelPath string // caminho relativo ao BaseDir (ex: "agent1/daily/2026-03-01T00-00-00-000.tar.gz")
}

// listLocalBackups percorre recursivamente baseDir e retorna todos os
// arquivos de backup (.tar.gz, .tar.zst), excluindo diretórios de chunks.
func listLocalBackups(baseDir string) ([]localBackupFile, error) {
	var files []localBackupFile

	err := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // ignora erros de permissão e continua
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), "chunks_") {
			return filepath.SkipDir
		}
		if !d.IsDir() && isBackupFile(d.Name()) {
			rel, relErr := filepath.Rel(baseDir, path)
			if relErr != nil {
				return nil // ignora se não conseguir calcular relativo
			}
			files = append(files, localBackupFile{
				AbsPath: path,
				RelPath: rel,
			})
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walking base dir %s: %w", baseDir, err)
	}

	// Ordena para processamento determinístico
	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})

	return files, nil
}

// uploadWithRetryStandalone é uma versão standalone do upload com retry,
// sem depender de um PostCommitOrchestrator instanciado.
func uploadWithRetryStandalone(ctx context.Context, backend objstore.Backend, localPath, remotePath string, maxRetry int, timeout time.Duration, logger *slog.Logger) error {
	var lastErr error
	for attempt := 0; attempt < maxRetry; attempt++ {
		uploadCtx, cancel := context.WithTimeout(ctx, timeout)
		err := backend.Upload(uploadCtx, localPath, remotePath)
		cancel()

		if err == nil {
			return nil
		}

		lastErr = err
		backoff := time.Duration(1<<uint(attempt*2)) * time.Second // 1s → 4s → 16s
		logger.Warn("sync-storage: upload attempt failed, retrying",
			"attempt", attempt+1,
			"max", maxRetry,
			"backoff", backoff,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}

	return fmt.Errorf("upload failed after %d attempts: %w", maxRetry, lastErr)
}
