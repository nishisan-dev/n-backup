// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// stall_reader.go implementa um io.Reader wrapper que detecta inatividade
// (stall) durante uploads. Se nenhum byte for lido em stallTimeout,
// o contexto é cancelado automaticamente. Enquanto bytes fluírem,
// o upload continua indefinidamente — sem deadline global.

package objstore

import (
	"io"
	"sync"
	"time"
)

// defaultStallTimeout é o tempo máximo de inatividade antes de cancelar.
// Pode ser overridden via configuração do bucket (stall_timeout).
const defaultStallTimeout = 5 * time.Minute

// stallDetectReader wraps an io.Reader and cancels a context if no data
// flows for longer than stallTimeout. Every successful Read resets the timer.
type stallDetectReader struct {
	inner        io.Reader
	stallTimeout time.Duration
	cancelFunc   func() // cancela o contexto do upload

	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
}

// newStallDetectReader cria um reader com detecção de stall.
// cancelFunc será chamado se a leitura parar por mais de stallTimeout.
func newStallDetectReader(inner io.Reader, stallTimeout time.Duration, cancelFunc func()) *stallDetectReader {
	if stallTimeout <= 0 {
		stallTimeout = defaultStallTimeout
	}

	r := &stallDetectReader{
		inner:        inner,
		stallTimeout: stallTimeout,
		cancelFunc:   cancelFunc,
	}

	r.timer = time.AfterFunc(stallTimeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.stopped {
			r.stopped = true
			r.cancelFunc()
		}
	})

	return r
}

// Read lê do reader interno e reseta o timer de inatividade a cada leitura
// bem-sucedida (n > 0). Se o reader retornar 0 bytes sem erro, o timer
// continua contando — eventual stall será detectado.
func (r *stallDetectReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.mu.Lock()
		if !r.stopped {
			r.timer.Reset(r.stallTimeout)
		}
		r.mu.Unlock()
	}
	return n, err
}

// Close para o timer de stall detection. Deve ser chamado após o upload
// completar (sucesso ou falha) para evitar leak da goroutine do timer.
func (r *stallDetectReader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.timer.Stop()
}
