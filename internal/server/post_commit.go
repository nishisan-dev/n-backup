// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// post_commit.go orquestra operações de Object Storage pós-commit:
// envia backups para buckets remotos conforme modo configurado (sync/offload/archive)
// com upload paralelo e retry com backoff exponencial.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
)

// PostCommitOrchestrator gerencia envio de backups para Object Storage após commit local.
type PostCommitOrchestrator struct {
	buckets  []bucketTarget
	logger   *slog.Logger
	timeout  time.Duration // timeout por upload (default: 30min)
	maxRetry int           // tentativas de retry (default: 3)
}

// bucketTarget agrupa config + backend instanciado para um bucket.
type bucketTarget struct {
	cfg     config.BucketConfig
	backend objstore.Backend
}

// PostCommitResult reporta o resultado das operações pós-commit.
type PostCommitResult struct {
	BucketName string
	Mode       string
	Success    bool
	Error      error
	Duration   time.Duration
}

// NewPostCommitOrchestrator cria um orchestrator a partir da lista de BucketConfigs.
// backendFactory permite injeção de mock em testes.
func NewPostCommitOrchestrator(
	buckets []config.BucketConfig,
	backendFactory func(cfg config.BucketConfig) (objstore.Backend, error),
	logger *slog.Logger,
) (*PostCommitOrchestrator, error) {
	if len(buckets) == 0 {
		return nil, nil // nenhum bucket = sem orquestrador
	}

	targets := make([]bucketTarget, 0, len(buckets))
	for _, bcfg := range buckets {
		backend, err := backendFactory(bcfg)
		if err != nil {
			return nil, fmt.Errorf("creating backend for bucket %q: %w", bcfg.Name, err)
		}
		targets = append(targets, bucketTarget{cfg: bcfg, backend: backend})
	}

	return &PostCommitOrchestrator{
		buckets:  targets,
		logger:   logger,
		timeout:  30 * time.Minute,
		maxRetry: 3,
	}, nil
}

// Execute processa os buckets pós-commit.
// finalPath: caminho do backup commitado.
// rotatedFiles: nomes dos arquivos removidos pelo Rotate (sem path completo).
// agentDir: diretório do agent onde reside o backup.
//
// Modos bloqueantes (offload) são aguardados antes do retorno.
// Modos não-bloqueantes (sync, archive) são executados em goroutines fire-and-forget.
func (o *PostCommitOrchestrator) Execute(ctx context.Context, finalPath string, rotatedFiles []string, agentDir string) []PostCommitResult {
	if o == nil || len(o.buckets) == 0 {
		return nil
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		results    []PostCommitResult
		blockingWg sync.WaitGroup
	)

	for _, bt := range o.buckets {
		bt := bt // capture para goroutine
		logger := o.logger.With("bucket", bt.cfg.Name, "mode", bt.cfg.Mode)

		// Offload bloqueia; sync e archive são fire-and-forget
		if bt.cfg.Mode == config.BucketModeOffload {
			blockingWg.Add(1)
		}
		wg.Add(1)

		go func() {
			defer wg.Done()
			if bt.cfg.Mode == config.BucketModeOffload {
				defer blockingWg.Done()
			}

			start := time.Now()
			var err error

			switch bt.cfg.Mode {
			case config.BucketModeSync:
				err = o.executeSync(ctx, bt, finalPath, rotatedFiles, logger)
			case config.BucketModeOffload:
				err = o.executeOffload(ctx, bt, finalPath, logger)
			case config.BucketModeArchive:
				err = o.executeArchive(ctx, bt, rotatedFiles, agentDir, logger)
			}

			result := PostCommitResult{
				BucketName: bt.cfg.Name,
				Mode:       bt.cfg.Mode,
				Success:    err == nil,
				Error:      err,
				Duration:   time.Since(start),
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()

			if err != nil {
				logger.Error("bucket operation failed", "error", err, "duration", result.Duration)
			} else {
				logger.Info("bucket operation completed", "duration", result.Duration)
			}
		}()
	}

	// Aguarda modos bloqueantes (offload)
	blockingWg.Wait()

	// Aguarda todos (incluindo fire-and-forget que possam ter terminado rápido)
	wg.Wait()

	return results
}

// HasBlockingBuckets retorna true se há pelo menos um bucket no modo offload.
func (o *PostCommitOrchestrator) HasBlockingBuckets() bool {
	if o == nil {
		return false
	}
	for _, bt := range o.buckets {
		if bt.cfg.Mode == config.BucketModeOffload {
			return true
		}
	}
	return false
}

// executeSync: upload do backup + delete espelhado dos rotated.
func (o *PostCommitOrchestrator) executeSync(ctx context.Context, bt bucketTarget, finalPath string, rotatedFiles []string, logger *slog.Logger) error {
	remotePath := bt.cfg.Prefix + filepath.Base(finalPath)

	// Upload do novo backup
	if err := o.uploadWithRetry(ctx, bt.backend, finalPath, remotePath, logger); err != nil {
		return fmt.Errorf("sync upload: %w", err)
	}

	// Espelhar deletes do Rotate local
	for _, name := range rotatedFiles {
		remoteKey := bt.cfg.Prefix + name
		if err := bt.backend.Delete(ctx, remoteKey); err != nil {
			logger.Warn("sync mirror delete failed (non-fatal)", "key", remoteKey, "error", err)
		} else {
			logger.Info("sync mirror deleted", "key", remoteKey)
		}
	}

	return nil
}

// executeOffload: upload + delete local + rotate no bucket via retain.
func (o *PostCommitOrchestrator) executeOffload(ctx context.Context, bt bucketTarget, finalPath string, logger *slog.Logger) error {
	remotePath := bt.cfg.Prefix + filepath.Base(finalPath)

	// Upload
	if err := o.uploadWithRetry(ctx, bt.backend, finalPath, remotePath, logger); err != nil {
		logger.Warn("offload upload failed — local file preserved", "error", err)
		return fmt.Errorf("offload upload: %w", err)
	}

	// Delete local
	if err := os.Remove(finalPath); err != nil {
		logger.Warn("offload: failed to remove local file", "path", finalPath, "error", err)
		// Não é erro fatal — o backup está safe no bucket
	} else {
		logger.Info("offload: local file removed", "path", finalPath)
	}

	// Rotate no bucket
	if err := o.rotateBucket(ctx, bt, logger); err != nil {
		logger.Warn("offload: bucket rotation failed (non-fatal)", "error", err)
	}

	return nil
}

// executeArchive: upload dos arquivos rotated + rotate no bucket via retain.
func (o *PostCommitOrchestrator) executeArchive(ctx context.Context, bt bucketTarget, rotatedFiles []string, agentDir string, logger *slog.Logger) error {
	if len(rotatedFiles) == 0 {
		logger.Info("archive: no rotated files to archive")
		return nil
	}

	for _, name := range rotatedFiles {
		localPath := filepath.Join(agentDir, name)
		remotePath := bt.cfg.Prefix + name

		// O arquivo pode já ter sido deletado pelo Rotate — tenta enviar se existir
		if _, err := os.Stat(localPath); os.IsNotExist(err) {
			logger.Warn("archive: rotated file not found (already deleted)", "file", name)
			continue
		}

		if err := o.uploadWithRetry(ctx, bt.backend, localPath, remotePath, logger); err != nil {
			logger.Warn("archive: upload failed for rotated file", "file", name, "error", err)
			continue // best-effort
		}
	}

	// Rotate no bucket
	if err := o.rotateBucket(ctx, bt, logger); err != nil {
		logger.Warn("archive: bucket rotation failed (non-fatal)", "error", err)
	}

	return nil
}

// rotateBucket aplica retain no bucket, removendo objetos excedentes.
func (o *PostCommitOrchestrator) rotateBucket(ctx context.Context, bt bucketTarget, logger *slog.Logger) error {
	objs, err := bt.backend.List(ctx, bt.cfg.Prefix)
	if err != nil {
		return fmt.Errorf("listing bucket objects: %w", err)
	}

	// Assume que os nomes contêm timestamp e já estão ordenados por chave
	sort.Slice(objs, func(i, j int) bool {
		return objs[i].Key < objs[j].Key
	})

	if len(objs) <= bt.cfg.Retain {
		return nil
	}

	toRemove := objs[:len(objs)-bt.cfg.Retain]
	for _, obj := range toRemove {
		if err := bt.backend.Delete(ctx, obj.Key); err != nil {
			logger.Warn("bucket rotation: delete failed", "key", obj.Key, "error", err)
		} else {
			logger.Info("bucket rotation: deleted old backup", "key", obj.Key)
		}
	}

	return nil
}

// uploadWithRetry tenta upload com retry exponencial (backoff 1s → 4s → 16s).
func (o *PostCommitOrchestrator) uploadWithRetry(ctx context.Context, backend objstore.Backend, localPath, remotePath string, logger *slog.Logger) error {
	var lastErr error
	for attempt := 0; attempt < o.maxRetry; attempt++ {
		uploadCtx, cancel := context.WithTimeout(ctx, o.timeout)
		err := backend.Upload(uploadCtx, localPath, remotePath)
		cancel()

		if err == nil {
			return nil
		}

		lastErr = err
		backoff := time.Duration(math.Pow(4, float64(attempt))) * time.Second
		logger.Warn("upload attempt failed, retrying",
			"attempt", attempt+1,
			"max", o.maxRetry,
			"backoff", backoff,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}

	return fmt.Errorf("upload failed after %d attempts: %w", o.maxRetry, lastErr)
}
