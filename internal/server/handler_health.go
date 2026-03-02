// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// handler_health.go contém o processamento de health check do server.
//
// Quando o agent (ou qualquer client) envia o magic "PING", o server responde
// com o status atual (ready) e o espaço em disco disponível. Esse fluxo é
// leve e idempotente — não altera nenhum estado interno.

package server

import (
	"log/slog"
	"net"

	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// handleHealthCheck processa um health check PING.
func (h *Handler) handleHealthCheck(conn net.Conn, logger *slog.Logger) {
	logger.Debug("health check received")

	// TODO: implementar disk free real com syscall.Statfs
	diskFree := uint64(0)

	if err := protocol.WriteHealthResponse(conn, protocol.HealthStatusReady, diskFree); err != nil {
		logger.Error("writing health response", "error", err)
	}
}
