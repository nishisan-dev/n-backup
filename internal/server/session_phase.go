// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// session_phase.go contém os structs atômicos para rastreamento de fase
// e progresso das etapas pós-streaming (verifying, uploading) de uma sessão.
//
// Os structs usam campos atômicos para leitura lock-free pelo endpoint HTTP
// de observabilidade, seguindo o mesmo padrão do SyncProgress em sync_storage.go.

package server

import (
	"sync/atomic"
	"time"
)

// SessionPhase constantes para as fases do ciclo de vida da sessão.
const (
	PhaseReceiving  = "receiving"
	PhaseAssembling = "assembling"
	PhaseVerifying  = "verifying"
	PhaseUploading  = "uploading"
	PhaseDone       = "done"
	PhaseFailed     = "failed"
)

// SessionPhaseTracker rastreia a fase atual de uma sessão de backup.
// Leitura lock-free via atomic.Value para uso pelo HTTP handler.
type SessionPhaseTracker struct {
	phase atomic.Value // string: receiving | assembling | verifying | uploading | done | failed
}

// NewSessionPhaseTracker cria um tracker com a fase inicial "receiving".
func NewSessionPhaseTracker() *SessionPhaseTracker {
	t := &SessionPhaseTracker{}
	t.phase.Store(PhaseReceiving)
	return t
}

// Set atualiza a fase atual da sessão.
func (t *SessionPhaseTracker) Set(phase string) {
	t.phase.Store(phase)
}

// Get retorna a fase atual da sessão.
func (t *SessionPhaseTracker) Get() string {
	if v := t.phase.Load(); v != nil {
		return v.(string)
	}
	return PhaseReceiving
}

// IntegrityProgress rastreia o progresso da verificação de integridade.
// Campos atômicos para leitura lock-free pelo endpoint HTTP.
type IntegrityProgress struct {
	BytesRead  atomic.Int64
	TotalBytes atomic.Int64
	Entries    atomic.Int64
	StartedAt  atomic.Value // time.Time
}

// NewIntegrityProgress cria um IntegrityProgress inicializado.
func NewIntegrityProgress(totalBytes int64) *IntegrityProgress {
	p := &IntegrityProgress{}
	p.TotalBytes.Store(totalBytes)
	p.StartedAt.Store(time.Now())
	return p
}

// PostCommitProgress rastreia o progresso do upload pós-commit.
// Campos atômicos para leitura lock-free pelo endpoint HTTP.
type PostCommitProgress struct {
	Bucket     atomic.Value // string — nome do bucket atual
	Mode       atomic.Value // string — sync | offload | archive
	BytesSent  atomic.Int64
	TotalBytes atomic.Int64
}

// NewPostCommitProgress cria um PostCommitProgress inicializado.
func NewPostCommitProgress() *PostCommitProgress {
	p := &PostCommitProgress{}
	p.Bucket.Store("")
	p.Mode.Store("")
	return p
}
