// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
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
	SessionsSnapshot() []SessionSummary
	SessionDetail(id string) (*SessionDetail, bool)
	ConnectedAgents() []AgentInfo
	StorageUsageSnapshot() []StorageUsage
	SessionHistorySnapshot() []SessionHistoryEntry
}

// MetricsData contém os dados de métricas coletados do Handler.
type MetricsData struct {
	TrafficIn   int64
	DiskWrite   int64
	ActiveConns int32
	Sessions    int
}

// NewRouter cria o http.Handler para a API de observabilidade e SPA.
// Aplica middleware ACL em todas as rotas.
func NewRouter(metrics HandlerMetrics, cfg *config.ServerConfig, acl *ACL, store *EventStore) http.Handler {
	mux := http.NewServeMux()

	// API v1
	mux.HandleFunc("GET /api/v1/health", handleHealth)
	mux.HandleFunc("GET /api/v1/metrics", makeMetricsHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions", makeSessionsHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions/{id}", makeSessionDetailHandler(metrics))
	mux.HandleFunc("GET /api/v1/agents", makeAgentsHandler(metrics))
	mux.HandleFunc("GET /api/v1/storages", makeStoragesHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions/history", makeSessionHistoryHandler(metrics))
	mux.HandleFunc("GET /api/v1/config/effective", makeConfigHandler(cfg))

	// Events endpoint (se store fornecido)
	if store != nil {
		mux.HandleFunc("GET /api/v1/events", makeEventsHandler(store))
	}

	// SPA — serve assets embarcados via go:embed
	spa := http.FileServer(WebFS())
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Para paths não-API que não existam como arquivo, serve index.html (SPA fallback)
		spa.ServeHTTP(w, r)
	}))

	return acl.Middleware(mux)
}

// handleHealth retorna status do processo, uptime, versão e métricas de runtime.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(startTime)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var lastPauseMs float64
	if mem.NumGC > 0 {
		// PauseNs é um ring buffer circular de 256 posições
		lastPauseMs = float64(mem.PauseNs[(mem.NumGC+255)%256]) / 1e6
	}

	resp := HealthResponse{
		Status:  "ok",
		Uptime:  uptime.String(),
		Version: Version,
		Go:      runtime.Version(),
		Stats: &ServerStats{
			GoRoutines:  runtime.NumGoroutine(),
			HeapAllocMB: float64(mem.HeapAlloc) / (1024 * 1024),
			HeapSysMB:   float64(mem.HeapSys) / (1024 * 1024),
			GCPauseMs:   lastPauseMs,
			GCCycles:    mem.NumGC,
			CPUCores:    runtime.NumCPU(),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// makeMetricsHandler retorna um handler que coleta métricas do Handler.
func makeMetricsHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := metrics.MetricsSnapshot()
		resp := MetricsResponse{
			TrafficInBytes: data.TrafficIn,
			DiskWriteBytes: data.DiskWrite,
			ActiveConns:    data.ActiveConns,
			Sessions:       data.Sessions,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// makeSessionsHandler retorna um handler que lista sessões ativas.
func makeSessionsHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessions := metrics.SessionsSnapshot()
		if sessions == nil {
			sessions = []SessionSummary{}
		}
		writeJSON(w, http.StatusOK, sessions)
	}
}

// makeSessionDetailHandler retorna um handler para detalhe de uma sessão por ID.
func makeSessionDetailHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}
		detail, found := metrics.SessionDetail(id)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

// makeConfigHandler retorna um handler com a config efetiva (sem segredos).
func makeConfigHandler(cfg *config.ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		storages := make(map[string]StorageSafe, len(cfg.Storages))
		for name, s := range cfg.Storages {
			storages[name] = StorageSafe{
				BaseDir:         s.BaseDir,
				MaxBackups:      s.MaxBackups,
				AssemblerMode:   s.AssemblerMode,
				CompressionMode: s.CompressionMode,
			}
		}

		resp := ConfigEffective{
			ServerListen: cfg.Server.Listen,
			WebUIListen:  cfg.WebUI.Listen,
			Storages:     storages,
			FlowRotation: FlowRotationSafe{
				Enabled:    cfg.FlowRotation.Enabled,
				MinMBps:    cfg.FlowRotation.MinMBps,
				EvalWindow: cfg.FlowRotation.EvalWindow.String(),
				Cooldown:   cfg.FlowRotation.Cooldown.String(),
			},
			LogLevel: cfg.Logging.Level,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// makeEventsHandler retorna um handler que serve os últimos eventos do store.
func makeEventsHandler(store *EventStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseInt(r.URL.Query().Get("limit"), 50)
		events := store.Recent(limit)
		writeJSON(w, http.StatusOK, events)
	}
}

// makeAgentsHandler retorna um handler que lista agentes conectados via control channel.
func makeAgentsHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agents := metrics.ConnectedAgents()
		if agents == nil {
			agents = []AgentInfo{}
		}
		writeJSON(w, http.StatusOK, agents)
	}
}

// makeStoragesHandler retorna um handler que lista storages com uso de disco.
func makeStoragesHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		storages := metrics.StorageUsageSnapshot()
		if storages == nil {
			storages = []StorageUsage{}
		}
		writeJSON(w, http.StatusOK, storages)
	}
}

// makeSessionHistoryHandler retorna um handler que lista sessões finalizadas.
func makeSessionHistoryHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		history := metrics.SessionHistorySnapshot()
		if history == nil {
			history = []SessionHistoryEntry{}
		}
		writeJSON(w, http.StatusOK, history)
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

// parseInt é um helper para parsear query params numéricos com default.
func parseInt(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		return defaultVal
	}
	return v
}
