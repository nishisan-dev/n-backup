// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import "sync"

// SessionHistoryRing é um ring buffer thread-safe para sessões finalizadas.
type SessionHistoryRing struct {
	mu  sync.RWMutex
	buf []SessionHistoryEntry
	pos int
	cap int
	len int
}

// NewSessionHistoryRing cria um ring buffer com capacidade fixa.
func NewSessionHistoryRing(capacity int) *SessionHistoryRing {
	if capacity <= 0 {
		capacity = 100
	}
	return &SessionHistoryRing{
		buf: make([]SessionHistoryEntry, capacity),
		cap: capacity,
	}
}

// Push adiciona uma entrada ao ring buffer.
func (r *SessionHistoryRing) Push(e SessionHistoryEntry) {
	r.mu.Lock()
	r.buf[r.pos] = e
	r.pos = (r.pos + 1) % r.cap
	if r.len < r.cap {
		r.len++
	}
	r.mu.Unlock()
}

// Recent retorna as últimas N entradas em ordem cronológica (mais antigo primeiro).
func (r *SessionHistoryRing) Recent(limit int) []SessionHistoryEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.len
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return []SessionHistoryEntry{}
	}

	result := make([]SessionHistoryEntry, n)
	start := (r.pos - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.cap]
	}
	return result
}
