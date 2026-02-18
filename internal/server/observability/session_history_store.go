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

// SessionHistoryStore combina ring in-memory com persistência JSONL para sessões finalizadas.
type SessionHistoryStore struct {
	ring      *SessionHistoryRing
	file      *os.File
	mu        sync.Mutex
	maxLines  int
	lineCount int
	path      string
}

// NewSessionHistoryStore cria store persistente para histórico de sessões finalizadas.
func NewSessionHistoryStore(path string, ringCap, maxLines int) (*SessionHistoryStore, error) {
	if maxLines <= 0 {
		maxLines = 5000
	}

	ring := NewSessionHistoryRing(ringCap)
	entries, lineCount, err := loadSessionHistoryJSONL(path)
	if err != nil {
		return nil, fmt.Errorf("loading session history file: %w", err)
	}

	start := 0
	if len(entries) > ringCap {
		start = len(entries) - ringCap
	}
	for _, e := range entries[start:] {
		ring.Push(e)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening session history file for append: %w", err)
	}

	return &SessionHistoryStore{ring: ring, file: f, maxLines: maxLines, lineCount: lineCount, path: path}, nil
}

func loadSessionHistoryJSONL(path string) ([]SessionHistoryEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var entries []SessionHistoryEntry
	lineCount := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineCount++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e SessionHistoryEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	return entries, lineCount, scanner.Err()
}

// Push persiste e guarda em memória uma sessão finalizada.
func (s *SessionHistoryStore) Push(e SessionHistoryEntry) {
	s.ring.Push(e)
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

// Recent retorna histórico recente.
func (s *SessionHistoryStore) Recent(limit int) []SessionHistoryEntry {
	return s.ring.Recent(limit)
}

// Close fecha handle de arquivo.
func (s *SessionHistoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *SessionHistoryStore) rotate() {
	keep := s.maxLines / 2
	entries, _, err := loadSessionHistoryJSONL(s.path)
	if err != nil || len(entries) <= keep {
		return
	}
	entries = entries[len(entries)-keep:]

	s.file.Close()
	f, err := os.Create(s.path)
	if err != nil {
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

	s.file, err = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	s.lineCount = len(entries)
}
