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

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}
