// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

// Package observability provê a SPA de observabilidade e APIs HTTP do nbackup-server.
package observability

import (
	"net"
	"net/http"
)

// ACL controla acesso HTTP por IP/CIDR.
// Funciona como deny-by-default: apenas IPs contidos em pelo menos um CIDR são permitidos.
type ACL struct {
	nets []*net.IPNet
}

// NewACL cria uma ACL a partir de CIDRs já parseados (vindos de config.WebUIConfig.ParsedCIDRs).
func NewACL(cidrs []*net.IPNet) *ACL {
	return &ACL{nets: cidrs}
}

// Middleware retorna um http.Handler que verifica o IP remoto contra a ACL.
// Se o IP não estiver em nenhum CIDR permitido, retorna 403 Forbidden.
func (a *ACL) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Allowed(r.RemoteAddr) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Allowed verifica se o endereço remoto (host:port) é permitido pela ACL.
func (a *ACL) Allowed(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Tenta tratar como IP puro (sem porta)
		host = remoteAddr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	for _, cidr := range a.nets {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
