// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"sync"
	"time"
)

// EventRing é um ring buffer thread-safe para eventos operacionais.
// Armazena os últimos N eventos, descartando os mais antigos quando cheio.
type EventRing struct {
	mu  sync.RWMutex
	buf []EventEntry
	pos int // próxima posição de escrita
	cap int
	len int // quantos slots estão ocupados (max = cap)
}

// NewEventRing cria um ring buffer com capacidade fixa.
func NewEventRing(capacity int) *EventRing {
	if capacity <= 0 {
		capacity = 100
	}
	return &EventRing{
		buf: make([]EventEntry, capacity),
		cap: capacity,
	}
}

// Push adiciona um evento ao buffer, num esquema circular.
func (r *EventRing) Push(e EventEntry) {
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

// Recent retorna os últimos N eventos em ordem cronológica (mais antigo primeiro).
// Se limit <= 0 ou > len, retorna todos os eventos disponíveis.
func (r *EventRing) Recent(limit int) []EventEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.len
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return []EventEntry{}
	}

	result := make([]EventEntry, n)
	// pos aponta para a PRÓXIMA posição de escrita.
	// O evento mais recente está em pos-1, o mais antigo em pos-len.
	start := (r.pos - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.cap]
	}

	return result
}

// Len retorna o número de eventos armazenados.
func (r *EventRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.len
}

// PushEvent é um helper para criar e inserir um evento com campos comuns.
func (r *EventRing) PushEvent(level, eventType, agent, message string, stream int) {
	r.Push(EventEntry{
		Level:   level,
		Type:    eventType,
		Agent:   agent,
		Stream:  stream,
		Message: message,
	})
}
