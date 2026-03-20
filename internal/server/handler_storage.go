// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_storage.go contém lógica de storage: scan de uso de disco,
// contagem de backups, scanner periódico com cache atômico e cleanup
// de sessões expiradas.
//
// O StorageScanner roda como goroutine background e popula um cache
// atômico para que a WebUI/API não precise executar syscall.Statfs +
// filepath.WalkDir a cada request HTTP.
//
// O CleanupExpiredSessions remove sessões parciais (single e parallel)
// que ultrapassaram o TTL de inatividade, liberando recursos e arquivos
// temporários no disco.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nishisan-dev/n-backup/internal/server/observability"
)

// StorageUsageSnapshot retorna uso de disco para cada storage configurado.
// Quando o scanner de storage está ativo (WebUI habilitada), retorna dados do cache.
// Quando não há cache (ex: WebUI desabilitada), faz scan síncrono.
// Implementa observability.HandlerMetrics.
func (h *Handler) StorageUsageSnapshot() []observability.StorageUsage {
	if cached := h.storageCache.Load(); cached != nil {
		return cached.([]observability.StorageUsage)
	}
	// Fallback: scan síncrono (geralmente só quando WebUI está desabilitada)
	return h.scanStorages()
}

// refreshStorageCache executa o scan de storages e armazena no cache atômico.
func (h *Handler) refreshStorageCache() {
	h.storageCache.Store(h.scanStorages())
}

// StartStorageScanner inicia goroutine que atualiza o cache de storage periodicamente.
// Faz scan imediato no início e depois repete a cada interval.
func (h *Handler) StartStorageScanner(ctx context.Context, interval time.Duration) {
	h.refreshStorageCache() // scan inicial síncrono

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.refreshStorageCache()
			}
		}
	}()
}

// scanStorages coleta uso de disco real (Statfs + WalkDir) para cada storage configurado.
func (h *Handler) scanStorages() []observability.StorageUsage {
	var result []observability.StorageUsage

	// Ordena nomes para output determinístico
	names := make([]string, 0, len(h.cfg.Storages))
	for name := range h.cfg.Storages {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		si := h.cfg.Storages[name]
		su := observability.StorageUsage{
			Name:            name,
			BaseDir:         si.BaseDir,
			MaxBackups:      si.MaxBackups,
			CompressionMode: si.CompressionMode,
			AssemblerMode:   si.AssemblerMode,
		}

		// Obtém uso de disco via Statfs
		var stat syscall.Statfs_t
		if err := syscall.Statfs(si.BaseDir, &stat); err == nil {
			su.TotalBytes = stat.Blocks * uint64(stat.Bsize)
			su.FreeBytes = stat.Bavail * uint64(stat.Bsize)
			su.UsedBytes = su.TotalBytes - (stat.Bfree * uint64(stat.Bsize))
			if su.TotalBytes > 0 {
				su.UsagePercent = float64(su.UsedBytes) / float64(su.TotalBytes) * 100.0
			}
		}

		// Conta backups existentes no diretório
		su.BackupsCount = countBackups(si.BaseDir)

		result = append(result, su)
	}

	return result
}

// countBackups conta recursivamente quantos arquivos de backup (.tar.gz / .tar.zst)
// existem em qualquer nível de profundidade abaixo de baseDir.
// Ignora diretórios de chunks temporários (chunks_*) para evitar percorrer
// a estrutura de sharding (256×256 subpastas) durante backups ativos.
func countBackups(baseDir string) int {
	count := 0
	_ = filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // ignora erros de permissão e continua
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), "chunks_") {
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(d.Name(), ".tar.gz") || strings.HasSuffix(d.Name(), ".tar.zst")) {
			count++
		}
		return nil
	})
	return count
}

// CleanupExpiredSessions remove sessões parciais expiradas e seus arquivos .tmp.
// O critério de expiração é baseado em LastActivity (último I/O bem-sucedido),
// não em CreatedAt, para evitar matar sessões ativas com backups grandes.
// Sessões expiradas são registradas no histórico e emitem evento para o dashboard.
func (h *Handler) CleanupExpiredSessions(ttl time.Duration, logger *slog.Logger) {
	h.sessions.Range(func(key, value any) bool {
		switch s := value.(type) {
		case *PartialSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			if time.Since(lastAct) > ttl {
				logger.Info("cleaning expired session",
					"session", key,
					"agent", s.AgentName,
					"storage", s.StorageName,
					"age", time.Since(s.CreatedAt).Round(time.Second),
					"idle", time.Since(lastAct).Round(time.Second),
				)
				h.recordSessionEnd(key.(string), s.AgentName, s.StorageName, s.BackupName, "single", s.CompressionMode, "expired", s.CreatedAt, s.BytesWritten.Load())
				if h.Events != nil {
					h.Events.PushEvent("error", "session_expired", s.AgentName, fmt.Sprintf("%s/%s expired (idle %s)", s.StorageName, s.BackupName, time.Since(lastAct).Round(time.Second)), 0)
				}
				os.Remove(s.TmpPath)
				h.sessions.Delete(key)
			}
		case *ParallelSession:
			lastAct := time.Unix(0, s.LastActivity.Load())
			if time.Since(lastAct) > ttl {
				logger.Info("cleaning expired parallel session",
					"session", key,
					"agent", s.AgentName,
					"storage", s.StorageName,
					"age", time.Since(s.CreatedAt).Round(time.Second),
					"idle", time.Since(lastAct).Round(time.Second),
				)
				h.recordSessionEnd(key.(string), s.AgentName, s.StorageName, s.BackupName, "parallel", s.StorageInfo.CompressionMode, "expired", s.CreatedAt, s.DiskWriteBytes.Load())
				if h.Events != nil {
					h.Events.PushEvent("error", "session_expired", s.AgentName, fmt.Sprintf("%s/%s expired (idle %s)", s.StorageName, s.BackupName, time.Since(lastAct).Round(time.Second)), 0)
				}
				s.Closing.Store(true)
				for _, slot := range s.Slots {
					if slot.CancelFn != nil {
						slot.CancelFn()
					}
					slot.ConnMu.Lock()
					if slot.Conn != nil {
						slot.Conn.Close()
					}
					slot.ConnMu.Unlock()
				}
				s.Assembler.Cleanup()
				h.sessions.Delete(key)
			}
		}
		return true
	})
}
