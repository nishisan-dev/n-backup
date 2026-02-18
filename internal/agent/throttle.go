// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// maxBurstSize é o tamanho máximo de burst para o rate limiter (256KB).
// Alinhado ao buffer de escrita do pipeline (bufio.NewWriterSize 256KB).
const maxBurstSize = 256 * 1024

// ThrottledWriter é um io.Writer com rate limiting baseado em token bucket.
// Limita a taxa de escrita a bytesPerSec bytes/segundo.
type ThrottledWriter struct {
	w       io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

// NewThrottledWriter cria um ThrottledWriter com a taxa máxima em bytes/segundo.
// Se bytesPerSec <= 0, retorna o writer original sem throttle (bypass).
func NewThrottledWriter(ctx context.Context, w io.Writer, bytesPerSec int64) io.Writer {
	if bytesPerSec <= 0 {
		return w
	}

	burst := int(bytesPerSec)
	if burst > maxBurstSize {
		burst = maxBurstSize
	}

	return &ThrottledWriter{
		w:       w,
		limiter: rate.NewLimiter(rate.Limit(bytesPerSec), burst),
		ctx:     ctx,
	}
}

// Write implementa io.Writer com rate limiting.
// Divide escritas maiores que o burst em pedaços para consumir tokens gradualmente.
func (tw *ThrottledWriter) Write(p []byte) (int, error) {
	totalWritten := 0

	for len(p) > 0 {
		// Limita cada pedaço ao burst size para evitar reservas enormes
		chunk := len(p)
		if chunk > tw.limiter.Burst() {
			chunk = tw.limiter.Burst()
		}

		// Espera tokens disponíveis (bloqueia respeitando o rate)
		if err := tw.limiter.WaitN(tw.ctx, chunk); err != nil {
			return totalWritten, err
		}

		n, err := tw.w.Write(p[:chunk])
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}

		p = p[n:]
	}

	return totalWritten, nil
}
