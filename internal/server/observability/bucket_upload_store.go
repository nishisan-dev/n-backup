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
	"time"
)

// BucketUploadEntry representa uma operação de upload pós-commit para Object Storage.
type BucketUploadEntry struct {
	Timestamp     string `json:"timestamp"`
	Agent         string `json:"agent"`
	Storage       string `json:"storage"`
	Backup        string `json:"backup,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	BucketName    string `json:"bucket_name"`
	Mode          string `json:"mode"` // sync | offload | archive
	Success       bool   `json:"success"`
	Error         string `json:"error,omitempty"`
	Duration      string `json:"duration"`
	BytesUploaded int64  `json:"bytes_uploaded,omitempty"`
}

// BucketUploadRing é um ring buffer thread-safe para histórico de uploads de bucket.
type BucketUploadRing struct {
	mu  sync.RWMutex
	buf []BucketUploadEntry
	pos int
	cap int
	len int
}

// NewBucketUploadRing cria um ring buffer com capacidade fixa.
func NewBucketUploadRing(capacity int) *BucketUploadRing {
	if capacity <= 0 {
		capacity = 200
	}
	return &BucketUploadRing{
		buf: make([]BucketUploadEntry, capacity),
		cap: capacity,
	}
}

// Push adiciona uma entrada ao ring buffer.
func (r *BucketUploadRing) Push(e BucketUploadEntry) {
	if e.Timestamp == "" {
		e.Timestamp = time.Now().Format(time.RFC3339)
	}
	r.mu.Lock()
	r.buf[r.pos] = e
	r.pos = (r.pos + 1) % r.cap
	if r.len < r.cap {
		r.len++
	}
	r.mu.Unlock()
}

// Recent retorna as últimas N entradas em ordem cronológica (mais antigo primeiro).
func (r *BucketUploadRing) Recent(limit int) []BucketUploadEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.len
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return []BucketUploadEntry{}
	}

	result := make([]BucketUploadEntry, n)
	start := (r.pos - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.cap]
	}
	return result
}

// Len retorna o número de entradas armazenadas.
func (r *BucketUploadRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.len
}

// BucketUploadStore combina ring in-memory com persistência JSONL para uploads de bucket.
type BucketUploadStore struct {
	ring      *BucketUploadRing
	file      *os.File
	mu        sync.Mutex
	maxLines  int
	lineCount int
	path      string
}

// NewBucketUploadStore cria store persistente para histórico de uploads de bucket.
func NewBucketUploadStore(path string, ringCap, maxLines int) (*BucketUploadStore, error) {
	if maxLines <= 0 {
		maxLines = 5000
	}

	ring := NewBucketUploadRing(ringCap)
	entries, lineCount, err := loadBucketUploadJSONL(path)
	if err != nil {
		return nil, fmt.Errorf("loading bucket upload file: %w", err)
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
		return nil, fmt.Errorf("opening bucket upload file for append: %w", err)
	}

	return &BucketUploadStore{ring: ring, file: f, maxLines: maxLines, lineCount: lineCount, path: path}, nil
}

func loadBucketUploadJSONL(path string) ([]BucketUploadEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var entries []BucketUploadEntry
	lineCount := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineCount++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e BucketUploadEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	return entries, lineCount, scanner.Err()
}

// Push persiste e guarda em memória um upload de bucket.
func (s *BucketUploadStore) Push(e BucketUploadEntry) {
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
func (s *BucketUploadStore) Recent(limit int) []BucketUploadEntry {
	return s.ring.Recent(limit)
}

// Close fecha handle de arquivo.
func (s *BucketUploadStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *BucketUploadStore) rotate() {
	keep := s.maxLines / 2
	entries, _, err := loadBucketUploadJSONL(s.path)
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
