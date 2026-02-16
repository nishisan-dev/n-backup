// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package config

import (
	"os"
	"path/filepath"
	"testing"
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

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}
