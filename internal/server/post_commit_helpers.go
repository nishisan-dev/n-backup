// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// defaultBackendFactory cria um objstore.Backend real a partir de BucketConfig.
// Resolve credenciais via variáveis de ambiente.
// É uma variável para permitir substituição em testes.
var defaultBackendFactory = func(cfg config.BucketConfig) (objstore.Backend, error) {
	accessKey := os.Getenv(cfg.Credentials.AccessKeyEnv)
	secretKey := os.Getenv(cfg.Credentials.SecretKeyEnv)

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credentials env vars %q/%q not set or empty",
			cfg.Credentials.AccessKeyEnv, cfg.Credentials.SecretKeyEnv)
	}

	return objstore.NewS3Backend(context.Background(), objstore.S3Config{
		Endpoint:     cfg.Endpoint,
		Region:       cfg.Region,
		Bucket:       cfg.Bucket,
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		StallTimeout: cfg.StallTimeout,
	})
}

// filterBucketsByMode retorna apenas os buckets que NÃO são do modo informado.
func filterBucketsExcluding(buckets []config.BucketConfig, excludeMode string) []config.BucketConfig {
	var result []config.BucketConfig
	for _, b := range buckets {
		if b.Mode != excludeMode {
			result = append(result, b)
		}
	}
	return result
}

// filterBucketsByMode retorna apenas os buckets do modo informado.
func filterBucketsByMode(buckets []config.BucketConfig, mode string) []config.BucketConfig {
	var result []config.BucketConfig
	for _, b := range buckets {
		if b.Mode == mode {
			result = append(result, b)
		}
	}
	return result
}

// hasArchiveBuckets retorna true se algum bucket é modo archive.
func hasArchiveBuckets(buckets []config.BucketConfig) bool {
	for _, b := range buckets {
		if b.Mode == config.BucketModeArchive {
			return true
		}
	}
	return false
}

// shouldAsyncUpload retorna true se TODOS os buckets não-archive têm async_upload habilitado.
// Se não há buckets ativos (excluindo archive), retorna false.
// Offload nunca será async (validação de config impede), mas o check é defensivo.
func shouldAsyncUpload(buckets []config.BucketConfig) bool {
	active := filterBucketsExcluding(buckets, config.BucketModeArchive)
	if len(active) == 0 {
		return false
	}
	for _, b := range active {
		if !b.AsyncUpload {
			return false
		}
	}
	return true
}

// BucketUploadContext carrega informações do caller para emissão de BucketUploadEntry.
type BucketUploadContext struct {
	Agent     string
	Storage   string
	Backup    string
	SessionID string
}

// bucketCtxFromSession cria BucketUploadContext a partir de uma PartialSession.
// Retorna contexto vazio se session é nil (resume path).
func bucketCtxFromSession(s *PartialSession) BucketUploadContext {
	if s == nil {
		return BucketUploadContext{}
	}
	return BucketUploadContext{
		Agent:   s.AgentName,
		Storage: s.StorageName,
		Backup:  s.BackupName,
	}
}

// runArchivePreRotate executa uploads de archive ANTES do Rotate local.
// Necessário porque o Rotate deleta os arquivos — archive precisa deles intactos.
// archiveCandidates: nomes dos arquivos que SERÃO deletados pelo Rotate.
func (h *Handler) runArchivePreRotate(storageInfo config.StorageInfo, archiveCandidates []string, agentDir string, bctx BucketUploadContext, logger *slog.Logger) {
	archiveBuckets := filterBucketsByMode(storageInfo.Buckets, config.BucketModeArchive)
	if len(archiveBuckets) == 0 || len(archiveCandidates) == 0 {
		return
	}

	o, err := NewPostCommitOrchestrator(archiveBuckets, defaultBackendFactory, logger)
	if err != nil {
		logger.Error("failed to create archive orchestrator", "error", err)
		return
	}
	if o == nil {
		return
	}

	// Para archive, finalPath não é usado (archive só envia rotatedFiles).
	// Passamos os candidates como rotatedFiles; eles ainda existem no disco.
	results := o.Execute(context.Background(), "", archiveCandidates, agentDir)
	h.logPostCommitResults(results, bctx, logger)
}

// runPostCommitSync executa operações de sync e offload pós-commit.
// Archive é excluído pois já foi tratado por runArchivePreRotate.
func (h *Handler) runPostCommitSync(storageInfo config.StorageInfo, finalPath string, rotatedFiles []string, agentDir string, bctx BucketUploadContext, logger *slog.Logger) {
	// Filtra archive — já tratado antes do Rotate
	buckets := filterBucketsExcluding(storageInfo.Buckets, config.BucketModeArchive)
	if len(buckets) == 0 {
		return
	}

	o, err := NewPostCommitOrchestrator(buckets, defaultBackendFactory, logger)
	if err != nil {
		logger.Error("failed to create post-commit orchestrator", "error", err)
		return
	}
	if o == nil {
		return
	}

	results := o.Execute(context.Background(), finalPath, rotatedFiles, agentDir)
	h.logPostCommitResults(results, bctx, logger)
}

// logPostCommitResults registra resultados e emite eventos para a WebUI.
func (h *Handler) logPostCommitResults(results []PostCommitResult, bctx BucketUploadContext, logger *slog.Logger) {
	for _, r := range results {
		if !r.Success {
			logger.Error("post-commit bucket operation failed",
				"bucket", r.BucketName,
				"mode", r.Mode,
				"error", r.Error,
				"duration", r.Duration,
			)
			if h.Events != nil {
				h.Events.PushEvent("error", "bucket_sync_failed", "",
					fmt.Sprintf("bucket %s (%s): %v", r.BucketName, r.Mode, r.Error), 0)
			}
		} else {
			if h.Events != nil {
				h.Events.PushEvent("info", "bucket_sync_ok", "",
					fmt.Sprintf("bucket %s (%s) completed in %s", r.BucketName, r.Mode, r.Duration.Round(1)), 0)
			}
		}

		// Registra no histórico de uploads de bucket
		if h.BucketUploads != nil {
			errStr := ""
			if r.Error != nil {
				errStr = r.Error.Error()
			}
			h.BucketUploads.Push(observability.BucketUploadEntry{
				Agent:      bctx.Agent,
				Storage:    bctx.Storage,
				Backup:     bctx.Backup,
				SessionID:  bctx.SessionID,
				BucketName: r.BucketName,
				Mode:       r.Mode,
				Success:    r.Success,
				Error:      errStr,
				Duration:   r.Duration.Truncate(time.Millisecond).String(),
			})
		}
	}
}

