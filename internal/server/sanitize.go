// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package server

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxPathComponentLength é o comprimento máximo permitido para nomes de agent, storage e backup.
const maxPathComponentLength = 255

// validatePathComponent valida que um nome (agentName, storageName, backupName) é seguro
// para uso como componente de caminho no filesystem. Previne path traversal.
func validatePathComponent(name, fieldName string) error {
	if name == "" {
		return fmt.Errorf("%s cannot be empty", fieldName)
	}

	if len(name) > maxPathComponentLength {
		return fmt.Errorf("%s exceeds max length %d", fieldName, maxPathComponentLength)
	}

	// Rejeita separadores de path
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%s contains path separator", fieldName)
	}

	// Rejeita NUL byte
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%s contains null byte", fieldName)
	}

	// Rejeita path traversal
	if name == "." || name == ".." || strings.HasPrefix(name, "..") {
		return fmt.Errorf("%s contains path traversal", fieldName)
	}

	// Rejeita nomes que começam com ponto (hidden files/dirs)
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%s starts with dot", fieldName)
	}

	return nil
}

// validatePathInBaseDir verifica que o caminho resolvido permanece dentro de baseDir.
// Defesa em profundidade contra path traversal.
func validatePathInBaseDir(baseDir, resolvedPath string) error {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("resolving base dir: %w", err)
	}
	absResolved, err := filepath.Abs(resolvedPath)
	if err != nil {
		return fmt.Errorf("resolving target path: %w", err)
	}

	// filepath.Rel retorna erro se os paths não compartilham prefixo
	rel, err := filepath.Rel(absBase, absResolved)
	if err != nil {
		return fmt.Errorf("path escapes base directory: %w", err)
	}

	// Se rel começa com "..", o path resolvido está fora de baseDir
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path %q escapes base directory %q", resolvedPath, baseDir)
	}

	return nil
}
