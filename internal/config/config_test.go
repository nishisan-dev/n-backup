// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAgentConfig_ExampleFile(t *testing.T) {
	cfgPath := filepath.Join("..", "..", "configs", "agent.example.yaml")
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("failed to load agent example config: %v", err)
	}

	if cfg.Agent.Name != "web-server-01" {
		t.Errorf("expected agent.name 'web-server-01', got %q", cfg.Agent.Name)
	}
	if cfg.Server.Address != "backup.nishisan.dev:9847" {
		t.Errorf("expected server address 'backup.nishisan.dev:9847', got %q", cfg.Server.Address)
	}
	if len(cfg.Backups) != 2 {
		t.Fatalf("expected 2 backup entries, got %d", len(cfg.Backups))
	}
	if cfg.Backups[0].Name != "app" {
		t.Errorf("expected backups[0].name 'app', got %q", cfg.Backups[0].Name)
	}
	if cfg.Backups[0].Storage != "scripts" {
		t.Errorf("expected backups[0].storage 'scripts', got %q", cfg.Backups[0].Storage)
	}
	if cfg.Backups[0].Schedule != "0 2 * * *" {
		t.Errorf("expected backups[0].schedule '0 2 * * *', got %q", cfg.Backups[0].Schedule)
	}
	if len(cfg.Backups[0].Sources) != 1 {
		t.Errorf("expected 1 source in backups[0], got %d", len(cfg.Backups[0].Sources))
	}
	if cfg.Backups[1].Name != "home" {
		t.Errorf("expected backups[1].name 'home', got %q", cfg.Backups[1].Name)
	}
	if cfg.Backups[1].Storage != "home-dirs" {
		t.Errorf("expected backups[1].storage 'home-dirs', got %q", cfg.Backups[1].Storage)
	}
	if cfg.Backups[1].Schedule != "0 */6 * * *" {
		t.Errorf("expected backups[1].schedule '0 */6 * * *', got %q", cfg.Backups[1].Schedule)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Errorf("expected max_attempts 5, got %d", cfg.Retry.MaxAttempts)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("expected logging level 'info', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.File != "/var/log/nbackup/agent.log" {
		t.Errorf("expected logging file '/var/log/nbackup/agent.log', got %q", cfg.Logging.File)
	}
	if cfg.Backups[0].Parallels != 0 {
		t.Errorf("expected backups[0].parallels 0, got %d", cfg.Backups[0].Parallels)
	}
	if cfg.Backups[1].Parallels != 4 {
		t.Errorf("expected backups[1].parallels 4, got %d", cfg.Backups[1].Parallels)
	}
	if !cfg.Backups[1].AutoScaler.IsEnabled() {
		t.Error("expected backups[1].auto_scaler.enabled true in example config")
	}
	if cfg.Backups[1].AutoScaler.Mode != "efficiency" {
		t.Errorf("expected backups[1].auto_scaler.mode 'efficiency', got %q", cfg.Backups[1].AutoScaler.Mode)
	}
	// backups[1] tem bandwidth_limit: "100mb" no example
	expectedBW := int64(100 * 1024 * 1024)
	if cfg.Backups[1].BandwidthLimitRaw != expectedBW {
		t.Errorf("expected backups[1].BandwidthLimitRaw %d, got %d", expectedBW, cfg.Backups[1].BandwidthLimitRaw)
	}
	// backups[0] não tem bandwidth_limit, deve ser 0
	if cfg.Backups[0].BandwidthLimitRaw != 0 {
		t.Errorf("expected backups[0].BandwidthLimitRaw 0, got %d", cfg.Backups[0].BandwidthLimitRaw)
	}
}

func TestLoadServerConfig_ExampleFile(t *testing.T) {
	cfgPath := filepath.Join("..", "..", "configs", "server.example.yaml")
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("failed to load server example config: %v", err)
	}

	if cfg.Server.Listen != "0.0.0.0:9847" {
		t.Errorf("expected listen '0.0.0.0:9847', got %q", cfg.Server.Listen)
	}

	scripts, ok := cfg.GetStorage("scripts")
	if !ok {
		t.Fatal("expected storage 'scripts' to exist")
	}
	if scripts.BaseDir != "/var/backups/scripts" {
		t.Errorf("expected scripts.base_dir '/var/backups/scripts', got %q", scripts.BaseDir)
	}
	if scripts.MaxBackups != 5 {
		t.Errorf("expected scripts.max_backups 5, got %d", scripts.MaxBackups)
	}
	if scripts.AssemblerMode != "eager" {
		t.Errorf("expected scripts.assembler_mode eager, got %q", scripts.AssemblerMode)
	}
	if scripts.AssemblerPendingMemRaw != 8*1024*1024 {
		t.Errorf("expected scripts.assembler_pending_mem_limit 8mb, got %d", scripts.AssemblerPendingMemRaw)
	}
	if scripts.ChunkShardLevels != 1 {
		t.Errorf("expected scripts.chunk_shard_levels 1, got %d", scripts.ChunkShardLevels)
	}
	if scripts.FsyncChunkWrites() {
		t.Errorf("expected scripts.chunk_fsync false (explicit), got true")
	}

	home, ok := cfg.GetStorage("home-dirs")
	if !ok {
		t.Fatal("expected storage 'home-dirs' to exist")
	}
	if home.BaseDir != "/var/backups/home" {
		t.Errorf("expected home-dirs.base_dir '/var/backups/home', got %q", home.BaseDir)
	}
	if home.MaxBackups != 10 {
		t.Errorf("expected home-dirs.max_backups 10, got %d", home.MaxBackups)
	}
	if home.AssemblerMode != "lazy" {
		t.Errorf("expected home-dirs.assembler_mode lazy, got %q", home.AssemblerMode)
	}
	if home.FsyncChunkWrites() {
		t.Errorf("expected home-dirs.chunk_fsync false (explicit), got true")
	}

	if cfg.WebUI.EventsFile != "/var/lib/nbackup/events.jsonl" {
		t.Errorf("expected web_ui.events_file set, got %q", cfg.WebUI.EventsFile)
	}
	if cfg.WebUI.SessionHistoryFile != "/var/lib/nbackup/session-history.jsonl" {
		t.Errorf("expected web_ui.session_history_file set, got %q", cfg.WebUI.SessionHistoryFile)
	}
	if cfg.WebUI.ActiveSessionsFile != "/var/lib/nbackup/active-sessions.jsonl" {
		t.Errorf("expected web_ui.active_sessions_file set, got %q", cfg.WebUI.ActiveSessionsFile)
	}
	if cfg.WebUI.ActiveSnapshotInterval != 5*time.Minute {
		t.Errorf("expected web_ui.active_snapshot_interval 5m, got %s", cfg.WebUI.ActiveSnapshotInterval)
	}
}

// validAgentYAML retorna um YAML mínimo válido para testes.
// Testes de validação podem substituir campos com writeTempConfig.
const validAgentYAML = `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    sources:
      - path: /tmp
`

func TestLoadAgentConfig_MissingName(t *testing.T) {
	content := `
agent:
  name: ""
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty agent.name")
	}
}

func TestLoadAgentConfig_MissingBackups(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups: []
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty backups")
	}
}

func TestLoadAgentConfig_MissingStorageName(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: ""
    schedule: "0 2 * * *"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty storage name")
	}
}

func TestLoadAgentConfig_MissingSchedule(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing schedule")
	}
}

func TestLoadAgentConfig_ParallelsOutOfRange(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    parallels: 256
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for parallels > 255")
	}
}

func TestLoadAgentConfig_ParallelsValid(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    parallels: 4
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Backups[0].Parallels != 4 {
		t.Errorf("expected parallels 4, got %d", cfg.Backups[0].Parallels)
	}
}

func TestLoadAgentConfig_AutoScalerLegacyString(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    auto_scaler: adaptive
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Backups[0].AutoScaler.Mode != "adaptive" {
		t.Errorf("expected auto_scaler.mode adaptive, got %q", cfg.Backups[0].AutoScaler.Mode)
	}
	if !cfg.Backups[0].AutoScaler.IsEnabled() {
		t.Error("expected legacy auto_scaler format to default enabled=true")
	}
}

func TestLoadAgentConfig_AutoScalerStructuredDisabled(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    auto_scaler:
      enabled: false
      mode: adaptive
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Backups[0].AutoScaler.Mode != "adaptive" {
		t.Errorf("expected auto_scaler.mode adaptive, got %q", cfg.Backups[0].AutoScaler.Mode)
	}
	if cfg.Backups[0].AutoScaler.IsEnabled() {
		t.Error("expected auto_scaler.enabled false")
	}
}

func TestLoadServerConfig_MissingListen(t *testing.T) {
	content := `
server:
  listen: ""
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty server.listen")
	}
}

func TestLoadServerConfig_MissingStorages(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages: {}
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty storages")
	}
}

func TestLoadServerConfig_DefaultMaxBackups(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 0
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.MaxBackups != 5 {
		t.Errorf("expected default max_backups 5, got %d", s.MaxBackups)
	}
	if s.AssemblerMode != "eager" {
		t.Errorf("expected default assembler_mode eager, got %q", s.AssemblerMode)
	}
	if s.AssemblerPendingMemRaw != 8*1024*1024 {
		t.Errorf("expected default assembler_pending_mem_limit 8mb, got %d", s.AssemblerPendingMemRaw)
	}
	if s.ChunkShardLevels != 1 {
		t.Errorf("expected default chunk_shard_levels 1, got %d", s.ChunkShardLevels)
	}
	if !s.FsyncChunkWrites() {
		t.Errorf("expected default chunk_fsync true (v4.0.0 default), got false")
	}
}

func TestLoadServerConfig_GapDetectionDeprecatedIgnored(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
gap_detection:
  enabled: true
  timeout: 120s
  check_interval: 10s
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("deprecated gap_detection should not cause errors, got: %v", err)
	}
	// Values are parsed but no defaults are applied — they will be ignored at runtime.
	// The presence of the section should not break loading.
	_ = cfg
}

func TestLoadServerConfig_InvalidAssemblerMode(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
    assembler_mode: "fast"
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid assembler_mode")
	}
}

func TestLoadServerConfig_InvalidAssemblerPendingMemLimit(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
    assembler_pending_mem_limit: "0mb"
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid assembler_pending_mem_limit")
	}
}

func TestLoadServerConfig_ChunkShardLevelsDefault(t *testing.T) {
	content := validServerYAMLBase
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.ChunkShardLevels != 1 {
		t.Errorf("expected default chunk_shard_levels 1, got %d", s.ChunkShardLevels)
	}
}

func TestLoadServerConfig_ChunkShardLevelsTwo(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
    chunk_shard_levels: 2
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.ChunkShardLevels != 2 {
		t.Errorf("expected chunk_shard_levels 2, got %d", s.ChunkShardLevels)
	}
}

func TestLoadServerConfig_ChunkFsyncEnabled(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
    chunk_fsync: true
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if !s.FsyncChunkWrites() {
		t.Errorf("expected chunk_fsync true, got false")
	}
}

func TestLoadServerConfig_InvalidChunkShardLevels(t *testing.T) {
	content := `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
    chunk_shard_levels: 5
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid chunk_shard_levels")
	}
}

func TestLoadAgentConfig_DefaultRetry(t *testing.T) {
	cfgPath := writeTempConfig(t, validAgentYAML)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Errorf("expected default max_attempts 5, got %d", cfg.Retry.MaxAttempts)
	}
}

func TestLoadAgentConfig_BandwidthLimitValid(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    bandwidth_limit: "50mb"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedBytes := int64(50 * 1024 * 1024)
	if cfg.Backups[0].BandwidthLimitRaw != expectedBytes {
		t.Errorf("expected BandwidthLimitRaw %d, got %d", expectedBytes, cfg.Backups[0].BandwidthLimitRaw)
	}
}

func TestLoadAgentConfig_BandwidthLimitDefault(t *testing.T) {
	cfgPath := writeTempConfig(t, validAgentYAML)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Sem bandwidth_limit configurado, deve ser 0 (sem limite)
	if cfg.Backups[0].BandwidthLimitRaw != 0 {
		t.Errorf("expected BandwidthLimitRaw 0 (no limit), got %d", cfg.Backups[0].BandwidthLimitRaw)
	}
}

func TestLoadAgentConfig_BandwidthLimitTooLow(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    bandwidth_limit: "32kb"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for bandwidth_limit below 64kb minimum")
	}
}

func TestLoadAgentConfig_BandwidthLimitInvalid(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    bandwidth_limit: "not-a-size"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid bandwidth_limit format")
	}
}

func TestLoadAgentConfig_ControlChannelKeepaliveTooLow(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
daemon:
  control_channel:
    keepalive_interval: 500ms
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for keepalive_interval < 1s")
	}
}

func TestLoadAgentConfig_FileNotFound(t *testing.T) {
	_, err := LoadAgentConfig("/nonexistent/path/agent.yaml")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestLoadAgentConfig_InvalidYAML(t *testing.T) {
	cfgPath := writeTempConfig(t, "{{invalid yaml}}")
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestGetStorage_NotFound(t *testing.T) {
	cfg := &ServerConfig{
		Storages: map[string]StorageInfo{
			"existing": {BaseDir: "/tmp", MaxBackups: 5},
		},
	}
	_, ok := cfg.GetStorage("nonexistent")
	if ok {
		t.Fatal("expected GetStorage to return false for nonexistent key")
	}
}

// --- WebUI Config Tests ---

const validServerYAMLBase = `
server:
  listen: "0.0.0.0:9847"
tls:
  ca_cert: /tmp/ca.pem
  server_cert: /tmp/server.pem
  server_key: /tmp/server-key.pem
storages:
  default:
    base_dir: /tmp/backups
    max_backups: 3
`

func TestLoadServerConfig_WebUI_EnabledNoOrigins(t *testing.T) {
	content := validServerYAMLBase + `
web_ui:
  enabled: true
  allow_origins: []
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for web_ui enabled with empty allow_origins")
	}
}

func TestLoadServerConfig_WebUI_EnabledWithCIDR(t *testing.T) {
	content := validServerYAMLBase + `
web_ui:
  enabled: true
  allow_origins:
    - "10.0.0.0/8"
    - "192.168.1.0/24"
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.WebUI.Enabled {
		t.Error("expected web_ui.enabled true")
	}
	if cfg.WebUI.Listen != "127.0.0.1:9848" {
		t.Errorf("expected default listen '127.0.0.1:9848', got %q", cfg.WebUI.Listen)
	}
	if len(cfg.WebUI.ParsedCIDRs) != 2 {
		t.Fatalf("expected 2 parsed CIDRs, got %d", len(cfg.WebUI.ParsedCIDRs))
	}
	if cfg.WebUI.ReadTimeout.Seconds() != 5 {
		t.Errorf("expected read_timeout 5s, got %v", cfg.WebUI.ReadTimeout)
	}
	if cfg.WebUI.WriteTimeout.Seconds() != 15 {
		t.Errorf("expected write_timeout 15s, got %v", cfg.WebUI.WriteTimeout)
	}
	if cfg.WebUI.IdleTimeout.Seconds() != 60 {
		t.Errorf("expected idle_timeout 60s, got %v", cfg.WebUI.IdleTimeout)
	}
}

func TestLoadServerConfig_WebUI_PureIP(t *testing.T) {
	content := validServerYAMLBase + `
web_ui:
  enabled: true
  allow_origins:
    - "192.168.1.10"
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.WebUI.ParsedCIDRs) != 1 {
		t.Fatalf("expected 1 parsed CIDR, got %d", len(cfg.WebUI.ParsedCIDRs))
	}
	// IP puro deve virar /32
	if cfg.WebUI.ParsedCIDRs[0].String() != "192.168.1.10/32" {
		t.Errorf("expected 192.168.1.10/32, got %s", cfg.WebUI.ParsedCIDRs[0].String())
	}
}

func TestLoadServerConfig_WebUI_InvalidOrigin(t *testing.T) {
	content := validServerYAMLBase + `
web_ui:
  enabled: true
  allow_origins:
    - "not-an-ip"
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid allow_origins entry")
	}
}

func TestLoadServerConfig_WebUI_Disabled(t *testing.T) {
	content := validServerYAMLBase + `
web_ui:
  enabled: false
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WebUI.Enabled {
		t.Error("expected web_ui.enabled false")
	}
	// Quando disabled, nenhum CIDR é parseado
	if len(cfg.WebUI.ParsedCIDRs) != 0 {
		t.Errorf("expected 0 parsed CIDRs when disabled, got %d", len(cfg.WebUI.ParsedCIDRs))
	}
}

// --- PortRotation Config Tests ---

func TestPortRotationConfig_EffectiveChunksPerCycle(t *testing.T) {
	tests := []struct {
		name     string
		cfg      PortRotationConfig
		expected int
	}{
		{
			name:     "per-n-chunks with valid cycle",
			cfg:      PortRotationConfig{Mode: "per-n-chunks", ChunksPerCycle: 100},
			expected: 100,
		},
		{
			name:     "per-n-chunks with zero cycle returns disabled",
			cfg:      PortRotationConfig{Mode: "per-n-chunks", ChunksPerCycle: 0},
			expected: 0,
		},
		{
			name:     "mode off with cycle set returns disabled",
			cfg:      PortRotationConfig{Mode: "off", ChunksPerCycle: 100},
			expected: 0,
		},
		{
			name:     "mode off without cycle returns disabled",
			cfg:      PortRotationConfig{Mode: "off", ChunksPerCycle: 0},
			expected: 0,
		},
		{
			name:     "empty mode with cycle set returns disabled",
			cfg:      PortRotationConfig{Mode: "", ChunksPerCycle: 50},
			expected: 0,
		},
		{
			name:     "empty mode without cycle returns disabled",
			cfg:      PortRotationConfig{Mode: "", ChunksPerCycle: 0},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.EffectiveChunksPerCycle()
			if got != tt.expected {
				t.Errorf("EffectiveChunksPerCycle() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestLoadAgentConfig_PortRotationPerNChunks(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    port_rotation:
      mode: "per-n-chunks"
      chunks_per_cycle: 200
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Backups[0].PortRotation.Mode != "per-n-chunks" {
		t.Errorf("expected port_rotation.mode 'per-n-chunks', got %q", cfg.Backups[0].PortRotation.Mode)
	}
	if cfg.Backups[0].PortRotation.EffectiveChunksPerCycle() != 200 {
		t.Errorf("expected EffectiveChunksPerCycle() 200, got %d", cfg.Backups[0].PortRotation.EffectiveChunksPerCycle())
	}
}

func TestLoadAgentConfig_PortRotationOff(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    port_rotation:
      mode: "off"
      chunks_per_cycle: 100
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Backups[0].PortRotation.Mode != "off" {
		t.Errorf("expected port_rotation.mode 'off', got %q", cfg.Backups[0].PortRotation.Mode)
	}
	if cfg.Backups[0].PortRotation.EffectiveChunksPerCycle() != 0 {
		t.Errorf("expected EffectiveChunksPerCycle() 0 when mode=off, got %d", cfg.Backups[0].PortRotation.EffectiveChunksPerCycle())
	}
}

func TestLoadAgentConfig_PortRotationDefault(t *testing.T) {
	cfgPath := writeTempConfig(t, validAgentYAML)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Sem port_rotation configurado, mode deve ser normalizado para "off"
	if cfg.Backups[0].PortRotation.Mode != "off" {
		t.Errorf("expected default port_rotation.mode 'off', got %q", cfg.Backups[0].PortRotation.Mode)
	}
	if cfg.Backups[0].PortRotation.EffectiveChunksPerCycle() != 0 {
		t.Errorf("expected EffectiveChunksPerCycle() 0 by default, got %d", cfg.Backups[0].PortRotation.EffectiveChunksPerCycle())
	}
}

func TestLoadAgentConfig_PortRotationInvalidMode(t *testing.T) {
	content := `
agent:
  name: "test-agent"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backups:
  - name: "test"
    storage: "default"
    schedule: "0 2 * * *"
    port_rotation:
      mode: "random"
    sources:
      - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid port_rotation.mode")
	}
}

func TestLoadServerConfig_BucketValidSync(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: s3-mirror
        provider: s3
        bucket: my-bucket
        mode: sync
        credentials:
          access_key_env: AWS_ACCESS_KEY_ID
          secret_key_env: AWS_SECRET_ACCESS_KEY
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if len(s.Buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(s.Buckets))
	}
	if s.Buckets[0].Mode != BucketModeSync {
		t.Errorf("expected mode sync, got %q", s.Buckets[0].Mode)
	}
	if s.Buckets[0].Region != "us-east-1" {
		t.Errorf("expected default region us-east-1, got %q", s.Buckets[0].Region)
	}
}

func TestLoadServerConfig_BucketMissingName(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - provider: s3
        bucket: my-bucket
        mode: sync
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing bucket name")
	}
}

func TestLoadServerConfig_BucketInvalidMode(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: test
        provider: s3
        bucket: my-bucket
        mode: magic
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid bucket mode")
	}
}

func TestLoadServerConfig_BucketSyncWithRetain(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: test
        provider: s3
        bucket: my-bucket
        mode: sync
        retain: 5
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for sync mode with retain set")
	}
}

func TestLoadServerConfig_BucketOffloadMissingRetain(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: test
        provider: s3
        bucket: my-bucket
        mode: offload
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for offload mode without retain")
	}
}

func TestLoadServerConfig_BucketArchiveValid(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: cold
        provider: s3
        bucket: cold-backups
        prefix: "scripts/"
        mode: archive
        retain: 30
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.Buckets[0].Retain != 30 {
		t.Errorf("expected retain 30, got %d", s.Buckets[0].Retain)
	}
}

func TestLoadServerConfig_BucketDuplicateName(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: dup
        provider: s3
        bucket: b1
        mode: sync
        credentials:
          access_key_env: A
          secret_key_env: B
      - name: dup
        provider: s3
        bucket: b2
        mode: offload
        retain: 5
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for duplicate bucket name")
	}
}

func TestLoadServerConfig_BucketMissingCredentials(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: test
        provider: s3
        bucket: my-bucket
        mode: sync
        credentials:
          access_key_env: ""
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty access_key_env")
	}
}

func TestLoadServerConfig_BucketInvalidProvider(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: test
        provider: gcs
        bucket: my-bucket
        mode: sync
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestLoadServerConfig_BucketSyncStrategyDefault(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: s3-mirror
        provider: s3
        bucket: my-bucket
        mode: sync
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.Buckets[0].SyncStrategy != SyncStrategySafe {
		t.Errorf("expected default sync_strategy safe, got %q", s.Buckets[0].SyncStrategy)
	}
}

func TestLoadServerConfig_BucketSyncStrategySpaceEfficient(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: s3-mirror
        provider: s3
        bucket: my-bucket
        mode: sync
        sync_strategy: space_efficient
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if s.Buckets[0].SyncStrategy != SyncStrategySpaceEfficient {
		t.Errorf("expected sync_strategy space_efficient, got %q", s.Buckets[0].SyncStrategy)
	}
}

func TestLoadServerConfig_BucketSyncStrategyInvalidValue(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: s3-mirror
        provider: s3
        bucket: my-bucket
        mode: sync
        sync_strategy: fast
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid sync_strategy value")
	}
}

func TestLoadServerConfig_BucketSyncStrategyOnOffloadRejected(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: offload-test
        provider: s3
        bucket: my-bucket
        mode: offload
        retain: 3
        sync_strategy: space_efficient
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for sync_strategy on offload mode")
	}
}

func TestLoadServerConfig_BucketAsyncUploadOnSync(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: s3-async
        provider: s3
        bucket: my-bucket
        mode: sync
        async_upload: true
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, _ := cfg.GetStorage("default")
	if !s.Buckets[0].AsyncUpload {
		t.Error("expected async_upload true")
	}
}

func TestLoadServerConfig_BucketAsyncUploadOnOffloadRejected(t *testing.T) {
	content := validServerYAMLBase + `
    buckets:
      - name: offload-async
        provider: s3
        bucket: my-bucket
        mode: offload
        retain: 3
        async_upload: true
        credentials:
          access_key_env: A
          secret_key_env: B
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for async_upload on offload mode")
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}
