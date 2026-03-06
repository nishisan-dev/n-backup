// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package objstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MockBackend implementa Backend para testes, armazenando objetos em memória.
type MockBackend struct {
	mu      sync.Mutex
	objects map[string]int64 // key → size

	UploadCalls []string // registra remotePaths enviados
	DeleteCalls []string // registra remotePaths deletados
	AbortCalls  []string // registra prefixos de abort chamados

	// Injeção de erros para testes de falha
	UploadErr error
	DeleteErr error
	ListErr   error
}

// NewMockBackend cria um mock backend vazio.
func NewMockBackend() *MockBackend {
	return &MockBackend{
		objects: make(map[string]int64),
	}
}

func (m *MockBackend) Upload(_ context.Context, localPath, remotePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.UploadErr != nil {
		return m.UploadErr
	}

	m.UploadCalls = append(m.UploadCalls, remotePath)
	m.objects[remotePath] = 0 // tamanho desconhecido no mock
	return nil
}

func (m *MockBackend) Delete(_ context.Context, remotePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.DeleteErr != nil {
		return m.DeleteErr
	}

	m.DeleteCalls = append(m.DeleteCalls, remotePath)
	delete(m.objects, remotePath)
	return nil
}

func (m *MockBackend) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ListErr != nil {
		return nil, m.ListErr
	}

	var result []ObjectInfo
	for key, size := range m.objects {
		if strings.HasPrefix(key, prefix) {
			result = append(result, ObjectInfo{Key: key, Size: size})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result, nil
}

// Seed popula o mock com objetos pré-existentes (para testes de retain/rotate).
func (m *MockBackend) Seed(keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		m.objects[k] = 100
	}
}

// ObjectKeys retorna as chaves atuais (sorted).
func (m *MockBackend) ObjectKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// AbortIncompleteUploads registra a chamada e retorna nil (mock no-op).
func (m *MockBackend) AbortIncompleteUploads(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AbortCalls = append(m.AbortCalls, prefix)
	return nil
}

// Compile-time interface check
var _ Backend = (*MockBackend)(nil)

// Compile-time interface check for S3Backend
var _ Backend = (*S3Backend)(nil)

// TestableBackendInterface verifica que MockBackend satisfaz a interface.
// Usado em testes para garantir que mudanças na interface são detectadas cedo.
func TestableBackendInterface() error {
	var b Backend = NewMockBackend()
	ctx := context.Background()

	if err := b.Upload(ctx, "/dev/null", "test/file.tar.gz"); err != nil {
		return fmt.Errorf("Upload: %w", err)
	}

	objs, err := b.List(ctx, "test/")
	if err != nil {
		return fmt.Errorf("List: %w", err)
	}
	if len(objs) != 1 {
		return fmt.Errorf("expected 1 object, got %d", len(objs))
	}

	if err := b.Delete(ctx, "test/file.tar.gz"); err != nil {
		return fmt.Errorf("Delete: %w", err)
	}

	objs, err = b.List(ctx, "test/")
	if err != nil {
		return fmt.Errorf("List after delete: %w", err)
	}
	if len(objs) != 0 {
		return fmt.Errorf("expected 0 objects after delete, got %d", len(objs))
	}

	return nil
}
