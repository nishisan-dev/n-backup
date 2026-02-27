// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
)

// mockMetrics implementa HandlerMetrics para testes.
type mockMetrics struct {
	data        MetricsData
	sessions    []SessionSummary
	details     map[string]*SessionDetail
	agents      []AgentInfo
	storages    []StorageUsage
	bufferStats *ChunkBufferDTO
}

func (m *mockMetrics) MetricsSnapshot() MetricsData       { return m.data }
func (m *mockMetrics) SessionsSnapshot() []SessionSummary { return m.sessions }
func (m *mockMetrics) SessionDetail(id string) (*SessionDetail, bool) {
	if m.details == nil {
		return nil, false
	}
	d, ok := m.details[id]
	return d, ok
}
func (m *mockMetrics) ConnectedAgents() []AgentInfo                  { return m.agents }
func (m *mockMetrics) StorageUsageSnapshot() []StorageUsage          { return m.storages }
func (m *mockMetrics) SessionHistorySnapshot() []SessionHistoryEntry { return nil }
func (m *mockMetrics) ActiveSessionHistorySnapshot(sessionID string, limit int) []ActiveSessionSnapshotEntry {
	return nil
}
func (m *mockMetrics) ChunkBufferStats() *ChunkBufferDTO { return m.bufferStats }

func newMockMetrics() *mockMetrics {
	return &mockMetrics{
		sessions: []SessionSummary{},
		details:  map[string]*SessionDetail{},
		agents:   []AgentInfo{},
		storages: []StorageUsage{},
	}
}

func testCfg() *config.ServerConfig {
	return &config.ServerConfig{
		Server:  config.ServerListen{Listen: "0.0.0.0:9847"},
		WebUI:   config.WebUIConfig{Listen: "127.0.0.1:9848"},
		Logging: config.LoggingInfo{Level: "info"},
		Storages: map[string]config.StorageInfo{
			"default": {BaseDir: "/tmp/backups", MaxBackups: 5, AssemblerMode: "eager"},
		},
		FlowRotation: config.FlowRotationConfig{Enabled: true, MinMBps: 1.0, EvalWindow: 60 * time.Minute, Cooldown: 15 * time.Minute},
	}
}

func localhostACL(t *testing.T) *ACL {
	t.Helper()
	return NewACL(parseCIDRs(t, "127.0.0.1/32"))
}

func TestHealth_ReturnsOK(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status 'ok', got %v", resp.Status)
	}
	if resp.Uptime == "" {
		t.Error("expected uptime field")
	}
	if resp.Version == "" {
		t.Error("expected version field")
	}

	// Fase 5: Server Stats
	if resp.Stats == nil {
		t.Fatal("expected stats field in health response")
	}
	if resp.Stats.GoRoutines <= 0 {
		t.Errorf("expected goroutines > 0, got %d", resp.Stats.GoRoutines)
	}
	if resp.Stats.CPUCores <= 0 {
		t.Errorf("expected cpu_cores > 0, got %d", resp.Stats.CPUCores)
	}
	if resp.Stats.HeapAllocMB <= 0 {
		t.Errorf("expected heap_alloc_mb > 0, got %f", resp.Stats.HeapAllocMB)
	}
}

func TestMetrics_ReturnsData(t *testing.T) {
	mock := newMockMetrics()
	mock.data = MetricsData{
		TrafficIn:   1024 * 1024,
		DiskWrite:   512 * 1024,
		ActiveConns: 3,
		Sessions:    2,
	}
	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp MetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.TrafficInBytes != 1024*1024 {
		t.Errorf("expected traffic_in_bytes %d, got %d", 1024*1024, resp.TrafficInBytes)
	}
	if resp.ActiveConns != 3 {
		t.Errorf("expected active_conns 3, got %d", resp.ActiveConns)
	}
	if resp.Sessions != 2 {
		t.Errorf("expected sessions 2, got %d", resp.Sessions)
	}
}

func TestPrometheusMetrics_ReturnsTextFormat(t *testing.T) {
	mock := newMockMetrics()
	mock.data = MetricsData{
		ActiveConns: 2,
		Sessions:    2,
		ChunkBuffer: &ChunkBufferDTO{
			Enabled:            true,
			CapacityBytes:      1024,
			InFlightBytes:      256,
			FillRatio:          0.25,
			TotalPushed:        10,
			TotalDrained:       8,
			TotalFallbacks:     1,
			BackpressureEvents: 3,
			DrainRatio:         0.8,
			DrainRateMBs:       12.5,
		},
	}
	mock.sessions = []SessionSummary{
		{SessionID: "single-1", Mode: "single", ActiveStreams: 1},
		{SessionID: "parallel-1", Mode: "parallel", ActiveStreams: 3},
	}
	mock.agents = []AgentInfo{
		{Name: "agent-a"},
		{Name: "agent-b"},
	}

	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", got)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"# HELP nbackup_server_active_connections",
		"nbackup_server_active_connections 2",
		"nbackup_server_active_sessions 2",
		"nbackup_server_active_sessions_by_mode{mode=\"single\"} 1",
		"nbackup_server_active_sessions_by_mode{mode=\"parallel\"} 1",
		"nbackup_server_active_streams 4",
		"nbackup_server_connected_agents 2",
		"nbackup_server_chunk_buffer_enabled 1",
		"nbackup_server_chunk_buffer_total_pushed_total 10",
		"nbackup_server_chunk_buffer_drain_mbps 12.5",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected metrics body to contain %q\nbody:\n%s", want, body)
		}
	}
}

func TestSessions_EmptyList(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty sessions, got %d", len(resp))
	}
}

func TestSessions_WithData(t *testing.T) {
	mock := newMockMetrics()
	mock.sessions = []SessionSummary{
		{SessionID: "abc123", Agent: "web-01", Mode: "parallel", ActiveStreams: 4, MaxStreams: 8, Status: "running"},
	}
	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 session, got %d", len(resp))
	}
	if resp[0].SessionID != "abc123" {
		t.Errorf("expected session abc123, got %s", resp[0].SessionID)
	}
}

func TestSessionDetail_NotFound(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/sessions/nonexistent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestSessionDetail_Found(t *testing.T) {
	mock := newMockMetrics()
	mock.details["abc123"] = &SessionDetail{
		SessionSummary: SessionSummary{
			SessionID: "abc123", Agent: "web-01", Mode: "parallel", Status: "running",
		},
		Streams: []StreamDetail{
			{Index: 0, OffsetBytes: 1024, MBps: 5.5},
			{Index: 1, OffsetBytes: 2048, MBps: 3.2},
		},
	}
	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/sessions/abc123", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp SessionDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.SessionID != "abc123" {
		t.Errorf("expected session abc123, got %s", resp.SessionID)
	}
	if len(resp.Streams) != 2 {
		t.Errorf("expected 2 streams, got %d", len(resp.Streams))
	}
}

func TestConfigEffective(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/config/effective", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp ConfigEffective
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.ServerListen != "0.0.0.0:9847" {
		t.Errorf("expected server_listen '0.0.0.0:9847', got %q", resp.ServerListen)
	}
	if resp.WebUIListen != "127.0.0.1:9848" {
		t.Errorf("expected web_ui_listen '127.0.0.1:9848', got %q", resp.WebUIListen)
	}
	if !resp.FlowRotation.Enabled {
		t.Error("expected flow_rotation.enabled true")
	}
	if _, ok := resp.Storages["default"]; !ok {
		t.Error("expected 'default' storage in config")
	}
}

func TestACL_BlocksHealthEndpoint(t *testing.T) {
	// ACL só permite 10.0.0.0/8
	acl := NewACL([]*net.IPNet{
		mustParseCIDR("10.0.0.0/8"),
	})
	router := NewRouter(newMockMetrics(), testCfg(), acl, nil)

	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.RemoteAddr = "192.168.1.1:12345" // não permitido
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestRoot_ReturnsSPA(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected Content-Type text/html, got %q", ct)
	}
}

func TestNotFound_Returns404(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/nonexistent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func mustParseCIDR(s string) *net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return cidr
}

func TestAgents_EmptyList(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []AgentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty agents, got %d", len(resp))
	}
}

func TestAgents_WithData(t *testing.T) {
	mock := newMockMetrics()
	mock.agents = []AgentInfo{
		{Name: "web-01", RemoteAddr: "10.0.0.1:54321", ConnectedAt: "2025-01-01T00:00:00Z", ConnectedFor: "1h0m0s", KeepaliveS: 30, HasSession: true, ClientVersion: "1.7.2"},
		{Name: "db-01", RemoteAddr: "10.0.0.2:54322", ConnectedAt: "2025-01-01T00:00:00Z", ConnectedFor: "30m0s", KeepaliveS: 15, HasSession: false},
	}
	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []AgentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(resp))
	}
	// Verifica pelo menos um campo
	found := false
	for _, a := range resp {
		if a.Name == "web-01" && a.HasSession && a.ClientVersion == "1.7.2" {
			found = true
		}
	}
	if !found {
		t.Error("expected agent web-01 with session and version 1.7.2")
	}
}

func TestStorages_EmptyList(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/storages", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []StorageUsage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected empty storages, got %d", len(resp))
	}
}

func TestStorages_WithData(t *testing.T) {
	mock := newMockMetrics()
	mock.storages = []StorageUsage{
		{
			Name:            "default",
			BaseDir:         "/tmp/backups",
			MaxBackups:      5,
			CompressionMode: "gzip",
			AssemblerMode:   "eager",
			TotalBytes:      100 * 1024 * 1024 * 1024,
			UsedBytes:       60 * 1024 * 1024 * 1024,
			FreeBytes:       40 * 1024 * 1024 * 1024,
			UsagePercent:    60.0,
			BackupsCount:    3,
		},
	}
	router := NewRouter(mock, testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/storages", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []StorageUsage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 storage, got %d", len(resp))
	}
	if resp[0].Name != "default" {
		t.Errorf("expected storage name 'default', got %q", resp[0].Name)
	}
	if resp[0].UsagePercent != 60.0 {
		t.Errorf("expected usage_percent 60.0, got %f", resp[0].UsagePercent)
	}
	if resp[0].BackupsCount != 3 {
		t.Errorf("expected backups_count 3, got %d", resp[0].BackupsCount)
	}
}

func TestActiveSessionHistory_ReturnsArray(t *testing.T) {
	router := NewRouter(newMockMetrics(), testCfg(), localhostACL(t), nil)

	req := httptest.NewRequest("GET", "/api/v1/sessions/active-history?limit=10", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp []ActiveSessionSnapshotEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("expected empty array, got %d", len(resp))
	}
}
