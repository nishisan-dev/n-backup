// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// Package objstore define a interface e implementações para Object Storage backends.
// Usado pelo PostCommitOrchestrator para enviar backups a destinos remotos
// após o commit local (sync, offload, archive).
package objstore

import (
	"context"
	"time"
)

// ObjectInfo representa metadados de um objeto no bucket.
type ObjectInfo struct {
	Key          string    // caminho/chave do objeto
	Size         int64     // tamanho em bytes
	LastModified time.Time // data de modificação
}

// Backend define a interface para providers de object storage.
// Cada implementação (S3, GCS, Azure) deve satisfazer esta interface.
type Backend interface {
	// Upload envia um arquivo local para o bucket no remotePath especificado.
	Upload(ctx context.Context, localPath, remotePath string) error

	// Delete remove um objeto do bucket.
	Delete(ctx context.Context, remotePath string) error

	// List lista objetos no bucket com o prefixo dado, ordenados por chave.
	// Usado para rotação via retain (offload/archive).
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}
