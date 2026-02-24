// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

// HealthResponse é retornado por GET /api/v1/health.
type HealthResponse struct {
	Status  string       `json:"status"`
	Uptime  string       `json:"uptime"`
	Version string       `json:"version"`
	Go      string       `json:"go"`
	Stats   *ServerStats `json:"stats,omitempty"`
}

// MetricsResponse é retornado por GET /api/v1/metrics.
type MetricsResponse struct {
	TrafficInBytes int64           `json:"traffic_in_bytes"`
	DiskWriteBytes int64           `json:"disk_write_bytes"`
	ActiveConns    int32           `json:"active_conns"`
	Sessions       int             `json:"sessions"`
	TrafficInMBps  float64         `json:"traffic_in_mbps,omitempty"` // preenchido se intervalo disponível
	DiskWriteMBps  float64         `json:"disk_write_mbps,omitempty"`
	ChunkBuffer    *ChunkBufferDTO `json:"chunk_buffer,omitempty"`
}

// SessionSummary é usado na lista de GET /api/v1/sessions.
type SessionSummary struct {
	SessionID      string `json:"session_id"`
	Agent          string `json:"agent"`
	Storage        string `json:"storage"`
	Backup         string `json:"backup,omitempty"`
	ClientVersion  string `json:"client_version"` // Versão do client
	Mode           string `json:"mode"`           // single | parallel
	Compression    string `json:"compression"`    // gzip | zst
	StartedAt      string `json:"started_at"`
	LastActivity   string `json:"last_activity"`
	BytesReceived  int64  `json:"bytes_received"`
	DiskWriteBytes int64  `json:"disk_write_bytes"`
	ActiveStreams  int    `json:"active_streams"`
	MaxStreams     int    `json:"max_streams,omitempty"`
	Status         string `json:"status"` // running | idle | degraded

	// Campos de progresso vindos do agent (via ControlProgress).
	// Zero values quando o agent não reporta progresso.
	TotalObjects uint32          `json:"total_objects,omitempty"`
	ObjectsSent  uint32          `json:"objects_sent,omitempty"`
	WalkComplete bool            `json:"walk_complete,omitempty"`
	ETA          string          `json:"eta,omitempty"` // "∞" até o agent reportar
	AssemblyETA  string          `json:"assembly_eta,omitempty"`
	Assembler    *AssemblerStats `json:"assembler,omitempty"`
	AutoScale    *AutoScaleInfo  `json:"auto_scale,omitempty"`

	// Campos de chunk buffer por sessão (zero quando buffer desabilitado).
	BufferEnabled       bool    `json:"buffer_enabled,omitempty"`
	BufferInFlightBytes int64   `json:"buffer_in_flight_bytes,omitempty"`
	BufferFillPercent   float64 `json:"buffer_fill_percent,omitempty"`
}

// AssemblerStats representa o estado do montador de chunks.
type AssemblerStats struct {
	NextExpectedSeq uint32 `json:"next_expected_seq"`
	PendingChunks   int    `json:"pending_chunks"`
	PendingMemBytes int64  `json:"pending_mem_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	Finalized       bool   `json:"finalized"`
	TotalChunks     uint32 `json:"total_chunks"`
	AssembledChunks uint32 `json:"assembled_chunks"`
	Phase           string `json:"phase"` // "receiving" | "assembling" | "done"
}

// SessionDetail é retornado por GET /api/v1/sessions/{id}.
type SessionDetail struct {
	SessionSummary
	Streams []StreamDetail `json:"streams,omitempty"`
}

// StreamDetail representa o estado de um stream individual dentro de uma sessão paralela.
type StreamDetail struct {
	Index        uint8   `json:"index"`
	OffsetBytes  int64   `json:"offset_bytes"`
	MBps         float64 `json:"mbps"`
	IdleSecs     int64   `json:"idle_secs"`
	SlowSince    string  `json:"slow_since,omitempty"`
	Active       bool    `json:"active"`
	Status       string  `json:"status"`                  // running | idle | slow | degraded | disconnected
	ConnectedFor string  `json:"connected_for,omitempty"` // ex: "2m30s", reseta na reconexão
	Reconnects   int32   `json:"reconnects"`              // 0 = primeira conexão, N = N reconexões
}

// EventEntry representa um evento operacional no ring buffer.
type EventEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"` // info | warn | error
	Type      string `json:"type"`  // reconnect | rotate | stream_dead | checksum_mismatch
	Agent     string `json:"agent,omitempty"`
	Stream    int    `json:"stream,omitempty"`
	Message   string `json:"message"`
}

// ConfigEffective é retornado por GET /api/v1/config/effective.
type ConfigEffective struct {
	ServerListen string                 `json:"server_listen"`
	WebUIListen  string                 `json:"web_ui_listen"`
	Storages     map[string]StorageSafe `json:"storages"`
	FlowRotation FlowRotationSafe       `json:"flow_rotation"`
	LogLevel     string                 `json:"log_level"`
}

// StorageSafe é uma visão segura (sem paths absolutos internos) de um storage.
type StorageSafe struct {
	BaseDir         string `json:"base_dir"`
	MaxBackups      int    `json:"max_backups"`
	AssemblerMode   string `json:"assembler_mode"`
	CompressionMode string `json:"compression_mode"`
}

// FlowRotationSafe é uma visão segura da config de flow rotation.
type FlowRotationSafe struct {
	Enabled    bool    `json:"enabled"`
	MinMBps    float64 `json:"min_mbps,omitempty"`
	EvalWindow string  `json:"eval_window,omitempty"`
	Cooldown   string  `json:"cooldown,omitempty"`
}

// AgentStats contém métricas de sistema do agente.
type AgentStats struct {
	CPUPercent       float32 `json:"cpu_percent"`
	MemoryPercent    float32 `json:"memory_percent"`
	DiskUsagePercent float32 `json:"disk_usage_percent"`
	LoadAverage      float32 `json:"load_average"`
}

// AutoScaleInfo contém métricas do auto-scaler recebidas do agent.
type AutoScaleInfo struct {
	Efficiency    float32 `json:"efficiency"`
	ProducerMBs   float32 `json:"producer_mbs"`
	DrainMBs      float32 `json:"drain_mbs"`
	ActiveStreams uint8   `json:"active_streams"`
	MaxStreams    uint8   `json:"max_streams"`
	State         string  `json:"state"` // stable | scaling_up | scaling_down | probing
	ProbeActive   bool    `json:"probe_active"`
}

// AgentInfo representa um agente conectado via control channel.
type AgentInfo struct {
	Name          string      `json:"name"`
	RemoteAddr    string      `json:"remote_addr"`
	ConnectedAt   string      `json:"connected_at"`
	ConnectedFor  string      `json:"connected_for"` // duração legível (ex: "1h23m")
	KeepaliveS    uint32      `json:"keepalive_s"`
	HasSession    bool        `json:"has_session"`
	ClientVersion string      `json:"client_version,omitempty"` // extraído da sessão, se houver
	Stats         *AgentStats `json:"stats,omitempty"`          // métricas atuais
}

// StorageUsage representa o uso de disco real de um storage.
type StorageUsage struct {
	Name            string  `json:"name"`
	BaseDir         string  `json:"base_dir"`
	MaxBackups      int     `json:"max_backups"`
	CompressionMode string  `json:"compression_mode"`
	AssemblerMode   string  `json:"assembler_mode"`
	TotalBytes      uint64  `json:"total_bytes"`
	UsedBytes       uint64  `json:"used_bytes"`
	FreeBytes       uint64  `json:"free_bytes"`
	UsagePercent    float64 `json:"usage_percent"`
	BackupsCount    int     `json:"backups_count"`
}

// ServerStats contém métricas de runtime do processo do server.
type ServerStats struct {
	GoRoutines  int     `json:"goroutines"`
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapSysMB   float64 `json:"heap_sys_mb"`
	GCPauseMs   float64 `json:"gc_pause_ms"`
	GCCycles    uint32  `json:"gc_cycles"`
	CPUCores    int     `json:"cpu_cores"`
}

// SessionHistoryEntry representa uma sessão de backup finalizada.
type SessionHistoryEntry struct {
	SessionID   string `json:"session_id"`
	Agent       string `json:"agent"`
	Storage     string `json:"storage"`
	Backup      string `json:"backup,omitempty"`
	Mode        string `json:"mode"`        // single | parallel
	Compression string `json:"compression"` // gzip | zstd
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	Duration    string `json:"duration"`
	BytesTotal  int64  `json:"bytes_total"`
	Result      string `json:"result"` // ok | checksum_mismatch | write_error | timeout | error
}

// ChunkBufferDTO representa o estado global do buffer de chunks em memória.
// Retornado dentro de MetricsResponse quando o buffer está habilitado.
type ChunkBufferDTO struct {
	Enabled            bool    `json:"enabled"`
	CapacityBytes      int64   `json:"capacity_bytes"`
	InFlightBytes      int64   `json:"in_flight_bytes"`
	FillRatio          float64 `json:"fill_ratio"`
	TotalPushed        int64   `json:"total_pushed"`
	TotalDrained       int64   `json:"total_drained"`
	TotalFallbacks     int64   `json:"total_fallbacks"`
	BackpressureEvents int64   `json:"backpressure_events"`
	DrainRatio         float64 `json:"drain_ratio"`
	DrainRateMBs       float64 `json:"drain_rate_mbs"` // MB/s taxa atual de drenagem (janela ~5s)
}
