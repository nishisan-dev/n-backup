// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/objstore"
)

// defaultBackendFactory cria um objstore.Backend real a partir de BucketConfig.
// Resolve credenciais via variáveis de ambiente.
func defaultBackendFactory(cfg config.BucketConfig) (objstore.Backend, error) {
	accessKey := os.Getenv(cfg.Credentials.AccessKeyEnv)
	secretKey := os.Getenv(cfg.Credentials.SecretKeyEnv)

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credentials env vars %q/%q not set or empty",
			cfg.Credentials.AccessKeyEnv, cfg.Credentials.SecretKeyEnv)
	}

	return objstore.NewS3Backend(context.Background(), objstore.S3Config{
		Endpoint:  cfg.Endpoint,
		Region:    cfg.Region,
		Bucket:    cfg.Bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
}

// runPostCommitSync executa as operações de Object Storage pós-commit.
// É no-op quando não há buckets configurados.
// Modos bloqueantes (offload) são aguardados antes do retorno.
func (h *Handler) runPostCommitSync(storageInfo config.StorageInfo, finalPath string, rotatedFiles []string, agentDir string, logger *slog.Logger) {
	if len(storageInfo.Buckets) == 0 {
		return
	}

	o, err := NewPostCommitOrchestrator(storageInfo.Buckets, defaultBackendFactory, logger)
	if err != nil {
		logger.Error("failed to create post-commit orchestrator", "error", err)
		return
	}
	if o == nil {
		return
	}

	results := o.Execute(context.Background(), finalPath, rotatedFiles, agentDir)
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
	}
}
