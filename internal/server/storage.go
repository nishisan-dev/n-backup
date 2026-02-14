// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AtomicWriter gerencia a escrita atômica de backups:
// grava em .tmp → valida → rename para nome final.
type AtomicWriter struct {
	baseDir    string
	agentName  string
	backupName string
	agentDir   string
}

// NewAtomicWriter cria um AtomicWriter para o agent e backup especificados.
// Cria o diretório {baseDir}/{agentName}/{backupName}/ se não existir.
func NewAtomicWriter(baseDir, agentName, backupName string) (*AtomicWriter, error) {
	agentDir := filepath.Join(baseDir, agentName, backupName)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("creating backup directory: %w", err)
	}
	return &AtomicWriter{
		baseDir:    baseDir,
		agentName:  agentName,
		backupName: backupName,
		agentDir:   agentDir,
	}, nil
}

// TempFile cria um arquivo temporário no diretório do agent.
func (w *AtomicWriter) TempFile() (*os.File, string, error) {
	f, err := os.CreateTemp(w.agentDir, "backup-*.tmp")
	if err != nil {
		return nil, "", fmt.Errorf("creating temp file: %w", err)
	}
	return f, f.Name(), nil
}

// Commit renomeia o arquivo temporário para o nome final com timestamp.
func (w *AtomicWriter) Commit(tmpPath string) (string, error) {
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05.000")
	// Substitui ponto decimal por traço para portabilidade em FS
	timestamp = strings.ReplaceAll(timestamp, ".", "-")
	finalName := fmt.Sprintf("%s.tar.gz", timestamp)
	finalPath := filepath.Join(w.agentDir, finalName)

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("renaming temp to final: %w", err)
	}

	return finalPath, nil
}

// Abort remove o arquivo temporário em caso de erro.
func (w *AtomicWriter) Abort(tmpPath string) error {
	return os.Remove(tmpPath)
}

// AgentDir retorna o caminho do diretório do agent.
func (w *AtomicWriter) AgentDir() string {
	return w.agentDir
}

// Rotate remove backups excedentes, mantendo os maxBackups mais recentes.
func Rotate(agentDir string, maxBackups int) error {
	if maxBackups <= 0 {
		return nil
	}

	entries, err := os.ReadDir(agentDir)
	if err != nil {
		return fmt.Errorf("reading agent directory: %w", err)
	}

	// Filtra apenas arquivos .tar.gz
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			backups = append(backups, e.Name())
		}
	}

	// Ordena por nome (timestamp → ordem cronológica natural)
	sort.Strings(backups)

	// Remove os mais antigos que excedam o limite
	if len(backups) > maxBackups {
		toRemove := backups[:len(backups)-maxBackups]
		for _, name := range toRemove {
			path := filepath.Join(agentDir, name)
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("removing old backup %s: %w", name, err)
			}
		}
	}

	return nil
}
