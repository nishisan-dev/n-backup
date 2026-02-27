// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"encoding/json"
	"fmt"
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
	ActiveSessionHistorySnapshot(sessionID string, limit int) []ActiveSessionSnapshotEntry
	ChunkBufferStats() *ChunkBufferDTO
}

// MetricsData contém os dados de métricas coletados do Handler.
type MetricsData struct {
	TrafficIn   int64
	DiskWrite   int64
	ActiveConns int32
	Sessions    int
	ChunkBuffer *ChunkBufferDTO
}

// NewRouter cria o http.Handler para a API de observabilidade e SPA.
// Aplica middleware ACL em todas as rotas.
func NewRouter(metrics HandlerMetrics, cfg *config.ServerConfig, acl *ACL, store *EventStore) http.Handler {
	mux := http.NewServeMux()

	// API v1
	mux.HandleFunc("GET /api/v1/health", handleHealth)
	mux.HandleFunc("GET /api/v1/metrics", makeMetricsHandler(metrics))
	mux.HandleFunc("GET /metrics", makePrometheusHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions", makeSessionsHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions/{id}", makeSessionDetailHandler(metrics))
	mux.HandleFunc("GET /api/v1/agents", makeAgentsHandler(metrics))
	mux.HandleFunc("GET /api/v1/storages", makeStoragesHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions/history", makeSessionHistoryHandler(metrics))
	mux.HandleFunc("GET /api/v1/sessions/active-history", makeActiveSessionHistoryHandler(metrics))
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
			ChunkBuffer:    data.ChunkBuffer,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// makePrometheusHandler retorna um handler que expõe métricas em formato texto
// compatível com Prometheus, sem depender de client_golang.
func makePrometheusHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := metrics.MetricsSnapshot()
		sessions := metrics.SessionsSnapshot()
		agents := metrics.ConnectedAgents()

		if sessions == nil {
			sessions = []SessionSummary{}
		}
		if agents == nil {
			agents = []AgentInfo{}
		}

		var singleSessions int
		var parallelSessions int
		var activeStreams int
		for _, s := range sessions {
			switch s.Mode {
			case "parallel":
				parallelSessions++
				activeStreams += s.ActiveStreams
			default:
				singleSessions++
				activeStreams++
			}
		}

		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintf(w, "# HELP nbackup_server_active_connections Active TLS backup/control connections currently open.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_active_connections gauge\n")
		fmt.Fprintf(w, "nbackup_server_active_connections %d\n", data.ActiveConns)

		fmt.Fprintf(w, "# HELP nbackup_server_active_sessions Active backup sessions currently tracked.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_active_sessions gauge\n")
		fmt.Fprintf(w, "nbackup_server_active_sessions %d\n", data.Sessions)

		fmt.Fprintf(w, "# HELP nbackup_server_active_sessions_by_mode Active sessions split by transfer mode.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_active_sessions_by_mode gauge\n")
		fmt.Fprintf(w, "nbackup_server_active_sessions_by_mode{mode=\"single\"} %d\n", singleSessions)
		fmt.Fprintf(w, "nbackup_server_active_sessions_by_mode{mode=\"parallel\"} %d\n", parallelSessions)

		fmt.Fprintf(w, "# HELP nbackup_server_active_streams Active data streams across all running sessions.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_active_streams gauge\n")
		fmt.Fprintf(w, "nbackup_server_active_streams %d\n", activeStreams)

		fmt.Fprintf(w, "# HELP nbackup_server_connected_agents Agents currently connected via control channel.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_connected_agents gauge\n")
		fmt.Fprintf(w, "nbackup_server_connected_agents %d\n", len(agents))

		fmt.Fprintf(w, "# HELP nbackup_server_runtime_goroutines Number of live goroutines.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_runtime_goroutines gauge\n")
		fmt.Fprintf(w, "nbackup_server_runtime_goroutines %d\n", runtime.NumGoroutine())

		fmt.Fprintf(w, "# HELP nbackup_server_runtime_heap_alloc_bytes Bytes of allocated heap objects.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_runtime_heap_alloc_bytes gauge\n")
		fmt.Fprintf(w, "nbackup_server_runtime_heap_alloc_bytes %d\n", mem.HeapAlloc)

		fmt.Fprintf(w, "# HELP nbackup_server_runtime_heap_sys_bytes Bytes of heap memory obtained from the OS.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_runtime_heap_sys_bytes gauge\n")
		fmt.Fprintf(w, "nbackup_server_runtime_heap_sys_bytes %d\n", mem.HeapSys)

		fmt.Fprintf(w, "# HELP nbackup_server_runtime_gc_cycles_total Total completed GC cycles.\n")
		fmt.Fprintf(w, "# TYPE nbackup_server_runtime_gc_cycles_total counter\n")
		fmt.Fprintf(w, "nbackup_server_runtime_gc_cycles_total %d\n", mem.NumGC)

		if data.ChunkBuffer != nil {
			cb := data.ChunkBuffer
			enabled := 0
			if cb.Enabled {
				enabled = 1
			}

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_enabled Whether the shared chunk buffer is enabled.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_enabled gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_enabled %d\n", enabled)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_capacity_bytes Configured chunk buffer capacity in bytes.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_capacity_bytes gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_capacity_bytes %d\n", cb.CapacityBytes)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_in_flight_bytes Bytes currently in flight inside the chunk buffer.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_in_flight_bytes gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_in_flight_bytes %d\n", cb.InFlightBytes)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_fill_ratio Fraction of buffer currently occupied.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_fill_ratio gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_fill_ratio %g\n", cb.FillRatio)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_total_pushed_total Total chunks pushed into the shared chunk buffer.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_total_pushed_total counter\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_total_pushed_total %d\n", cb.TotalPushed)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_total_drained_total Total chunks drained from the shared chunk buffer.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_total_drained_total counter\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_total_drained_total %d\n", cb.TotalDrained)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_fallbacks_total Total synchronous fallbacks when chunk buffer push failed.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_fallbacks_total counter\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_fallbacks_total %d\n", cb.TotalFallbacks)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_backpressure_events_total Total backpressure events observed in the chunk buffer.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_backpressure_events_total counter\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_backpressure_events_total %d\n", cb.BackpressureEvents)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_drain_ratio Ratio of drained chunks to pushed chunks.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_drain_ratio gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_drain_ratio %g\n", cb.DrainRatio)

			fmt.Fprintf(w, "# HELP nbackup_server_chunk_buffer_drain_mbps Current chunk buffer drain rate in MB/s.\n")
			fmt.Fprintf(w, "# TYPE nbackup_server_chunk_buffer_drain_mbps gauge\n")
			fmt.Fprintf(w, "nbackup_server_chunk_buffer_drain_mbps %g\n", cb.DrainRateMBs)
		}
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

// makeActiveSessionHistoryHandler retorna snapshots históricos de sessões ativas.
func makeActiveSessionHistoryHandler(metrics HandlerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseInt(r.URL.Query().Get("limit"), 120)
		sessionID := r.URL.Query().Get("session_id")
		history := metrics.ActiveSessionHistorySnapshot(sessionID, limit)
		if history == nil {
			history = []ActiveSessionSnapshotEntry{}
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
