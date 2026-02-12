// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/pki"
	"github.com/nishisan-dev/n-backup/internal/protocol"
)

// RunBackup executa uma sessão completa de backup para um BackupEntry:
// 1. Conecta ao server via mTLS
// 2. Envia handshake (agent name + storage name) e recebe ACK
// 3. Faz streaming dos dados (tar.gz)
// 4. Envia trailer com checksum e recebe Final ACK
func RunBackup(ctx context.Context, cfg *config.AgentConfig, entry config.BackupEntry, logger *slog.Logger) error {
	logger = logger.With("backup", entry.Name, "storage", entry.Storage)
	logger.Info("starting backup session", "server", cfg.Server.Address)

	// Configura TLS
	tlsCfg, err := pki.NewClientTLSConfig(cfg.TLS.CACert, cfg.TLS.ClientCert, cfg.TLS.ClientKey)
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	// Conecta ao server
	conn, err := dialWithContext(ctx, cfg.Server.Address, tlsCfg)
	if err != nil {
		return fmt.Errorf("connecting to server: %w", err)
	}
	defer conn.Close()

	logger.Info("connected to server", "address", cfg.Server.Address)

	// Fase 1: Handshake
	if err := protocol.WriteHandshake(conn, cfg.Agent.Name, entry.Storage); err != nil {
		return fmt.Errorf("writing handshake: %w", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		return fmt.Errorf("reading handshake ACK: %w", err)
	}

	if ack.Status != protocol.StatusGo {
		return fmt.Errorf("server rejected backup: status=%d message=%q", ack.Status, ack.Message)
	}

	logger.Info("handshake successful, starting data transfer")

	// Fase 2: Stream dados
	sources := make([]string, len(entry.Sources))
	for i, s := range entry.Sources {
		sources[i] = s.Path
	}

	scanner := NewScanner(sources, entry.Exclude)
	result, err := Stream(ctx, scanner, conn)
	if err != nil {
		return fmt.Errorf("streaming data: %w", err)
	}

	logger.Info("data transfer complete",
		"bytes", result.Size,
		"checksum", fmt.Sprintf("%x", result.Checksum),
	)

	// Fase 3: Trailer
	if err := protocol.WriteTrailer(conn, result.Checksum, result.Size); err != nil {
		return fmt.Errorf("writing trailer: %w", err)
	}

	finalACK, err := protocol.ReadFinalACK(conn)
	if err != nil {
		return fmt.Errorf("reading final ACK: %w", err)
	}

	switch finalACK.Status {
	case protocol.FinalStatusOK:
		logger.Info("backup completed successfully",
			"bytes", result.Size,
		)
		return nil
	case protocol.FinalStatusChecksumMismatch:
		return fmt.Errorf("server reported checksum mismatch")
	case protocol.FinalStatusWriteError:
		return fmt.Errorf("server reported write error")
	default:
		return fmt.Errorf("server returned unknown status: %d", finalACK.Status)
	}
}

// dialWithContext conecta via TLS respeitando o contexto para cancelamento.
func dialWithContext(ctx context.Context, address string, tlsCfg *tls.Config) (*tls.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(conn, tlsCfg)

	// Realiza o handshake TLS com deadline baseado no context
	if deadline, ok := ctx.Deadline(); ok {
		tlsConn.SetDeadline(deadline)
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	// Remove deadline após handshake (o stream não deve ter timeout fixo)
	tlsConn.SetDeadline(time.Time{})

	return tlsConn, nil
}