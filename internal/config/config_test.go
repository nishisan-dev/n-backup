package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAgentConfig_ExampleFile(t *testing.T) {
	// Localiza o arquivo de exemplo relativo à raiz do projeto
	cfgPath := filepath.Join("..", "..", "configs", "agent.example.yaml")
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("failed to load agent example config: %v", err)
	}

	if cfg.Agent.Name != "web-server-01" {
		t.Errorf("expected agent.name 'web-server-01', got %q", cfg.Agent.Name)
	}
	if cfg.Daemon.Schedule != "0 2 * * *" {
		t.Errorf("expected schedule '0 2 * * *', got %q", cfg.Daemon.Schedule)
	}
	if cfg.Server.Address != "backup.nishisan.dev:9847" {
		t.Errorf("expected server address 'backup.nishisan.dev:9847', got %q", cfg.Server.Address)
	}
	if len(cfg.Backup.Sources) != 3 {
		t.Errorf("expected 3 backup sources, got %d", len(cfg.Backup.Sources))
	}
	if len(cfg.Backup.Exclude) != 5 {
		t.Errorf("expected 5 excludes, got %d", len(cfg.Backup.Exclude))
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Errorf("expected max_attempts 5, got %d", cfg.Retry.MaxAttempts)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("expected logging level 'info', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("expected logging format 'json', got %q", cfg.Logging.Format)
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
	if cfg.Storage.BaseDir != "/var/backups/nbackup" {
		t.Errorf("expected base_dir '/var/backups/nbackup', got %q", cfg.Storage.BaseDir)
	}
	if cfg.Storage.MaxBackups != 5 {
		t.Errorf("expected max_backups 5, got %d", cfg.Storage.MaxBackups)
	}
}

func TestLoadAgentConfig_MissingName(t *testing.T) {
	content := `
agent:
  name: ""
daemon:
  schedule: "0 2 * * *"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backup:
  sources:
    - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty agent.name")
	}
}

func TestLoadAgentConfig_MissingSources(t *testing.T) {
	content := `
agent:
  name: "test-agent"
daemon:
  schedule: "0 2 * * *"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backup:
  sources: []
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadAgentConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty sources")
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
storage:
  base_dir: /tmp/backups
  max_backups: 3
`
	cfgPath := writeTempConfig(t, content)
	_, err := LoadServerConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for empty server.listen")
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
storage:
  base_dir: /tmp/backups
  max_backups: 0
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadServerConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Storage.MaxBackups != 5 {
		t.Errorf("expected default max_backups 5, got %d", cfg.Storage.MaxBackups)
	}
}

func TestLoadAgentConfig_DefaultRetry(t *testing.T) {
	content := `
agent:
  name: "test-agent"
daemon:
  schedule: "0 2 * * *"
server:
  address: "localhost:9847"
tls:
  ca_cert: /tmp/ca.pem
  client_cert: /tmp/client.pem
  client_key: /tmp/client-key.pem
backup:
  sources:
    - path: /tmp
`
	cfgPath := writeTempConfig(t, content)
	cfg, err := LoadAgentConfig(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Retry.MaxAttempts != 5 {
		t.Errorf("expected default max_attempts 5, got %d", cfg.Retry.MaxAttempts)
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

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}
