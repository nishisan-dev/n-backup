// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

// HealthResponse é retornado por GET /api/v1/health.
type HealthResponse struct {
	Status  string `json:"status"`
	Uptime  string `json:"uptime"`
	Version string `json:"version"`
	Go      string `json:"go"`
}

// MetricsResponse é retornado por GET /api/v1/metrics.
type MetricsResponse struct {
	TrafficInBytes int64   `json:"traffic_in_bytes"`
	DiskWriteBytes int64   `json:"disk_write_bytes"`
	ActiveConns    int32   `json:"active_conns"`
	Sessions       int     `json:"sessions"`
	TrafficInMBps  float64 `json:"traffic_in_mbps,omitempty"` // preenchido se intervalo disponível
	DiskWriteMBps  float64 `json:"disk_write_mbps,omitempty"`
}

// SessionSummary é usado na lista de GET /api/v1/sessions.
type SessionSummary struct {
	SessionID      string `json:"session_id"`
	Agent          string `json:"agent"`
	Storage        string `json:"storage"`
	Backup         string `json:"backup,omitempty"`
	ClientVersion  string `json:"client_version"` // Versão do client
	Mode           string `json:"mode"`           // single | parallel
	StartedAt      string `json:"started_at"`
	LastActivity   string `json:"last_activity"`
	BytesReceived  int64  `json:"bytes_received"`
	DiskWriteBytes int64  `json:"disk_write_bytes"`
	ActiveStreams  int    `json:"active_streams"`
	MaxStreams     int    `json:"max_streams,omitempty"`
	Status         string `json:"status"` // running | idle | degraded

	// Campos de progresso vindos do agent (via ControlProgress).
	// Zero values quando o agent não reporta progresso.
	TotalObjects uint32 `json:"total_objects,omitempty"`
	ObjectsSent  uint32 `json:"objects_sent,omitempty"`
	WalkComplete bool   `json:"walk_complete,omitempty"`
	ETA          string `json:"eta,omitempty"` // "∞" até o agent reportar
}

// SessionDetail é retornado por GET /api/v1/sessions/{id}.
type SessionDetail struct {
	SessionSummary
	Streams []StreamDetail `json:"streams,omitempty"`
}

// StreamDetail representa o estado de um stream individual dentro de uma sessão paralela.
type StreamDetail struct {
	Index       uint8   `json:"index"`
	OffsetBytes int64   `json:"offset_bytes"`
	MBps        float64 `json:"mbps"`
	IdleSecs    int64   `json:"idle_secs"`
	SlowSince   string  `json:"slow_since,omitempty"`
	Active      bool    `json:"active"`
	Status      string  `json:"status"` // running | idle | slow | degraded | inactive
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
	BaseDir       string `json:"base_dir"`
	MaxBackups    int    `json:"max_backups"`
	AssemblerMode string `json:"assembler_mode"`
}

// FlowRotationSafe é uma visão segura da config de flow rotation.
type FlowRotationSafe struct {
	Enabled    bool    `json:"enabled"`
	MinMBps    float64 `json:"min_mbps,omitempty"`
	EvalWindow string  `json:"eval_window,omitempty"`
	Cooldown   string  `json:"cooldown,omitempty"`
}
