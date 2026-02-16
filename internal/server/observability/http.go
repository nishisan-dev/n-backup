// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

// startTime registra quando o processo iniciou (para cálculo de uptime).
var startTime = time.Now()

// Version é preenchida via ldflags no build (-X ...Version=x.y.z).
var Version = "dev"

// HandlerMetrics define a interface read-only que o router precisa do server.Handler.
// Isso desacopla o pacote observability do server sem expor o Handler inteiro.
type HandlerMetrics interface {
	MetricsSnapshot() MetricsData
}

// MetricsData contém os dados de métricas coletados do Handler.
type MetricsData struct {
	TrafficIn   int64
	DiskWrite   int64
	ActiveConns int32
}

// NewRouter cria o http.Handler para a API de observabilidade e SPA.
// Aplica middleware ACL em todas as rotas.
func NewRouter(metrics HandlerMetrics, cfg *config.ServerConfig, acl *ACL) http.Handler {
	mux := http.NewServeMux()

	// API v1
	mux.HandleFunc("GET /api/v1/health", handleHealth)
	mux.HandleFunc("GET /api/v1/metrics", makeMetricsHandler(metrics))

	// SPA root (placeholder até Fase 4)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>NBackup Observability</title></head><body><h1>NBackup Server</h1><p>SPA em construção.</p></body></html>`))
	})

	return acl.Middleware(mux)
}

// handleHealth retorna status do processo, uptime e versão.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(startTime)
	resp := map[string]interface{}{
		"status":  "ok",
		"uptime":  uptime.String(),
		"version": Version,
		"go":      runtime.Version(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// makeMetricsHandler retorna um handler que coleta métricas do Handler.
func makeMetricsHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := metrics.MetricsSnapshot()
		resp := map[string]interface{}{
			"traffic_in_bytes": data.TrafficIn,
			"disk_write_bytes": data.DiskWrite,
			"active_conns":     data.ActiveConns,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// writeJSON serializa v como JSON e envia com status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
