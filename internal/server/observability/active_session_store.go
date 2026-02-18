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

// ActiveSessionSnapshotEntry representa um snapshot periódico de uma sessão ativa.
type ActiveSessionSnapshotEntry struct {
	Timestamp      string `json:"timestamp"`
	SessionID      string `json:"session_id"`
	Agent          string `json:"agent"`
	Storage        string `json:"storage"`
	Backup         string `json:"backup,omitempty"`
	Mode           string `json:"mode"`
	Compression    string `json:"compression"`
	BytesReceived  int64  `json:"bytes_received"`
	DiskWriteBytes int64  `json:"disk_write_bytes"`
	ActiveStreams  int    `json:"active_streams"`
	MaxStreams     int    `json:"max_streams,omitempty"`
	Status         string `json:"status"`
}

// ActiveSessionStore mantém snapshots de sessões ativas em ring + JSONL.
type ActiveSessionStore struct {
	ring      *activeSessionRing
	file      *os.File
	mu        sync.Mutex
	maxLines  int
	lineCount int
	path      string
}

type activeSessionRing struct {
	mu  sync.RWMutex
	buf []ActiveSessionSnapshotEntry
	pos int
	cap int
	len int
}

func newActiveSessionRing(capacity int) *activeSessionRing {
	if capacity <= 0 {
		capacity = 1000
	}
	return &activeSessionRing{buf: make([]ActiveSessionSnapshotEntry, capacity), cap: capacity}
}

func (r *activeSessionRing) Push(e ActiveSessionSnapshotEntry) {
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

func (r *activeSessionRing) Recent(limit int) []ActiveSessionSnapshotEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.len
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return []ActiveSessionSnapshotEntry{}
	}

	result := make([]ActiveSessionSnapshotEntry, n)
	start := (r.pos - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.cap]
	}
	return result
}

// NewActiveSessionStore cria store persistente para snapshots de sessão.
func NewActiveSessionStore(path string, ringCap, maxLines int) (*ActiveSessionStore, error) {
	if maxLines <= 0 {
		maxLines = 20000
	}

	ring := newActiveSessionRing(ringCap)
	entries, lineCount, err := loadActiveSessionJSONL(path)
	if err != nil {
		return nil, fmt.Errorf("loading active sessions file: %w", err)
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
		return nil, fmt.Errorf("opening active sessions file for append: %w", err)
	}

	return &ActiveSessionStore{ring: ring, file: f, maxLines: maxLines, lineCount: lineCount, path: path}, nil
}

func loadActiveSessionJSONL(path string) ([]ActiveSessionSnapshotEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	var entries []ActiveSessionSnapshotEntry
	lineCount := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineCount++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ActiveSessionSnapshotEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	return entries, lineCount, scanner.Err()
}

// PushSnapshot converte SessionSummary em snapshot e persiste.
func (s *ActiveSessionStore) PushSnapshot(summary SessionSummary, ts time.Time) {
	e := ActiveSessionSnapshotEntry{
		Timestamp:      ts.Format(time.RFC3339),
		SessionID:      summary.SessionID,
		Agent:          summary.Agent,
		Storage:        summary.Storage,
		Backup:         summary.Backup,
		Mode:           summary.Mode,
		Compression:    summary.Compression,
		BytesReceived:  summary.BytesReceived,
		DiskWriteBytes: summary.DiskWriteBytes,
		ActiveStreams:  summary.ActiveStreams,
		MaxStreams:     summary.MaxStreams,
		Status:         summary.Status,
	}
	s.push(e)
}

func (s *ActiveSessionStore) push(e ActiveSessionSnapshotEntry) {
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

// Recent retorna snapshots recentes, opcionalmente filtrados por sessão.
func (s *ActiveSessionStore) Recent(limit int, sessionID string) []ActiveSessionSnapshotEntry {
	items := s.ring.Recent(0)
	if sessionID == "" {
		if limit > 0 && len(items) > limit {
			return items[len(items)-limit:]
		}
		return items
	}

	filtered := make([]ActiveSessionSnapshotEntry, 0, len(items))
	for _, item := range items {
		if item.SessionID == sessionID {
			filtered = append(filtered, item)
		}
	}
	if limit > 0 && len(filtered) > limit {
		return filtered[len(filtered)-limit:]
	}
	return filtered
}

// Close fecha o arquivo JSONL.
func (s *ActiveSessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *ActiveSessionStore) rotate() {
	keep := s.maxLines / 2
	entries, _, err := loadActiveSessionJSONL(s.path)
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
