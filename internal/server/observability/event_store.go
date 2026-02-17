// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// EventStore combina um EventRing (in-memory) com persistência em arquivo JSONL.
// Cada Push() faz append de uma linha JSON ao arquivo. No startup, as últimas
// entradas são carregadas para popular o ring buffer.
//
// Rotação: quando o arquivo excede maxLines, reescreve mantendo as últimas
// maxLines/2 linhas. Isso evita crescimento indefinido sem perder histórico recente.
type EventStore struct {
	ring      *EventRing
	file      *os.File
	mu        sync.Mutex // protege writes e rotação no arquivo
	maxLines  int
	lineCount int
	path      string
}

// NewEventStore abre (ou cria) o arquivo JSONL e carrega as últimas entradas
// para popular o ring buffer. ringCap define a capacidade do ring in-memory,
// maxLines define quando o arquivo será rotacionado.
func NewEventStore(path string, ringCap, maxLines int) (*EventStore, error) {
	if maxLines <= 0 {
		maxLines = 10000
	}

	ring := NewEventRing(ringCap)

	// Carrega eventos existentes do arquivo
	entries, lineCount, err := loadJSONL(path)
	if err != nil {
		return nil, fmt.Errorf("loading events file: %w", err)
	}

	// Popula o ring com as últimas entradas (limitado por ringCap)
	start := 0
	if len(entries) > ringCap {
		start = len(entries) - ringCap
	}
	for _, e := range entries[start:] {
		ring.Push(e)
	}

	// Abre o arquivo para append
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening events file for append: %w", err)
	}

	return &EventStore{
		ring:      ring,
		file:      f,
		maxLines:  maxLines,
		lineCount: lineCount,
		path:      path,
	}, nil
}

// loadJSONL lê o arquivo JSONL e retorna todos os EventEntry válidos.
// Linhas malformadas são ignoradas silenciosamente.
func loadJSONL(path string) ([]EventEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var entries []EventEntry
	lineCount := 0
	scanner := bufio.NewScanner(f)
	// Aumenta o buffer do scanner para linhas longas (1MB)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineCount++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e EventEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // ignora linhas corrompidas
		}
		entries = append(entries, e)
	}

	return entries, lineCount, scanner.Err()
}

// Push adiciona um evento ao ring buffer e persiste no arquivo JSONL.
func (s *EventStore) Push(e EventEntry) {
	s.ring.Push(e) // ring preenche timestamp se vazio

	// Re-lê do ring para pegar o timestamp preenchido
	recent := s.ring.Recent(1)
	if len(recent) == 0 {
		return
	}
	filled := recent[0]

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(filled)
	if err != nil {
		return
	}

	if _, err := s.file.Write(append(data, '\n')); err != nil {
		return
	}

	s.lineCount++
	if s.lineCount > s.maxLines {
		s.rotate()
	}
}

// PushEvent é um helper para criar e inserir um evento com campos comuns.
func (s *EventStore) PushEvent(level, eventType, agent, message string, stream int) {
	s.Push(EventEntry{
		Level:   level,
		Type:    eventType,
		Agent:   agent,
		Stream:  stream,
		Message: message,
	})
}

// Recent retorna os últimos N eventos em ordem cronológica (mais antigo primeiro).
func (s *EventStore) Recent(limit int) []EventEntry {
	return s.ring.Recent(limit)
}

// Len retorna o número de eventos no ring buffer in-memory.
func (s *EventStore) Len() int {
	return s.ring.Len()
}

// Close fecha o file handle do arquivo JSONL.
func (s *EventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// rotate mantém as últimas maxLines/2 linhas do arquivo.
// Deve ser chamada com s.mu já travado.
func (s *EventStore) rotate() {
	keep := s.maxLines / 2

	// Lê todas as linhas do arquivo
	entries, _, err := loadJSONL(s.path)
	if err != nil || len(entries) <= keep {
		return
	}

	// Mantém as últimas 'keep' entradas
	entries = entries[len(entries)-keep:]

	// Fecha o arquivo atual
	s.file.Close()

	// Reescreve o arquivo com as entradas mantidas
	f, err := os.Create(s.path)
	if err != nil {
		// Tenta reabrir em modo append para não perder o handle
		s.file, _ = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		return
	}

	w := bufio.NewWriter(f)
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		w.Write(data)
		w.WriteByte('\n')
	}
	w.Flush()
	f.Close()

	// Reabre em modo append
	s.file, err = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	s.lineCount = len(entries)
}
