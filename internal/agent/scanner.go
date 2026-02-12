// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// Package agent implementa o client de backup (nbackup-agent).
package agent

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Scanner caminha pelos diretórios de origem e filtra arquivos
// conforme as regras de exclude (glob patterns).
type Scanner struct {
	sources  []string
	excludes []string
}

// NewScanner cria um Scanner com os sources e excludes fornecidos.
func NewScanner(sources []string, excludes []string) *Scanner {
	return &Scanner{
		sources:  sources,
		excludes: excludes,
	}
}

// FileEntry representa um arquivo encontrado pelo scanner.
type FileEntry struct {
	// Path é o caminho absoluto do arquivo no sistema de origem.
	Path string
	// RelPath é o caminho relativo (para uso no tar).
	RelPath string
	// Info contém metadados do arquivo.
	Info fs.FileInfo
}

// Scan itera sobre todos os arquivos elegíveis e chama fn para cada um.
// O contexto permite cancelamento durante o scan.
func (s *Scanner) Scan(ctx context.Context, fn func(entry FileEntry) error) error {
	for _, src := range s.sources {
		// Normaliza o source path
		src = filepath.Clean(src)

		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				// Pula arquivos inacessíveis
				return nil
			}

			// Verifica cancelamento
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			// Calcula caminho relativo ao root (/) para manter estrutura
			relPath := strings.TrimPrefix(path, "/")

			// Verifica excludes
			if s.isExcluded(relPath, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Obtém FileInfo
			info, err := d.Info()
			if err != nil {
				return nil // pula se não conseguir obter info
			}

			return fn(FileEntry{
				Path:    path,
				RelPath: relPath,
				Info:    info,
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// isExcluded verifica se o caminho relativo corresponde a algum glob de exclusão.
// Suporta:
//   - "*.log"              → match pelo basename
//   - ".git/**"            → match diretório em qualquer nível
//   - "*/access-logs/"     → trailing slash indica match de diretório
//   - "node_modules/**"    → exclui diretório e todo conteúdo
func (s *Scanner) isExcluded(relPath string, isDir bool) bool {
	base := filepath.Base(relPath)
	parts := strings.Split(relPath, string(os.PathSeparator))

	for _, pattern := range s.excludes {
		// Trailing slash = match apenas diretórios pelo nome
		if strings.HasSuffix(pattern, "/") {
			if isDir {
				dirPattern := strings.TrimSuffix(pattern, "/")
				// Remove */ prefix se existir (ex: "*/access-logs/" → "access-logs")
				dirPattern = strings.TrimPrefix(dirPattern, "*/")
				for _, part := range parts {
					if matched, _ := filepath.Match(dirPattern, part); matched {
						return true
					}
				}
			}
			continue
		}

		// Patterns com "/**" suffix — exclui diretório e todo conteúdo recursivamente
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			for _, part := range parts {
				if matched, _ := filepath.Match(prefix, part); matched {
					return true
				}
			}
			continue
		}

		// Testa o caminho completo contra o pattern
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}

		// Testa o basename contra o pattern (ex: "*.log" matcha qualquer .log)
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

// ScanStats contém o resultado de um pré-scan rápido (sem I/O de leitura).
type ScanStats struct {
	TotalBytes   int64
	TotalObjects int64
}

// PreScan faz um walk rápido para contar bytes e objetos elegíveis.
// Usado para calcular ETA e barra de progresso proporcional.
func (s *Scanner) PreScan(ctx context.Context) (*ScanStats, error) {
	stats := &ScanStats{}
	for _, src := range s.sources {
		src = filepath.Clean(src)
		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			relPath := strings.TrimPrefix(path, "/")
			if s.isExcluded(relPath, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			stats.TotalObjects++
			if d.Type().IsRegular() {
				info, err := d.Info()
				if err == nil {
					stats.TotalBytes += info.Size()
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return stats, nil
}
