package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig representa a configuração completa do nbackup-agent.
type AgentConfig struct {
	Agent   AgentInfo     `yaml:"agent"`
	Daemon  DaemonInfo    `yaml:"daemon"`
	Server  ServerAddr    `yaml:"server"`
	TLS     TLSClient     `yaml:"tls"`
	Backups []BackupEntry `yaml:"backups"`
	Retry   RetryInfo     `yaml:"retry"`
	Logging LoggingInfo   `yaml:"logging"`
}

// AgentInfo identifica o agent.
type AgentInfo struct {
	Name string `yaml:"name"`
}

// DaemonInfo contém a cron expression do scheduler.
type DaemonInfo struct {
	Schedule string `yaml:"schedule"`
}

// ServerAddr contém o endereço do servidor de backup.
type ServerAddr struct {
	Address string `yaml:"address"`
}

// TLSClient contém os caminhos dos certificados mTLS do client.
type TLSClient struct {
	CACert     string `yaml:"ca_cert"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
}

// BackupEntry representa um bloco de backup nomeado com storage de destino.
type BackupEntry struct {
	Name    string         `yaml:"name"`    // Identificador local do backup
	Storage string         `yaml:"storage"` // Nome do storage no server
	Sources []BackupSource `yaml:"sources"`
	Exclude []string       `yaml:"exclude"`
}

// BackupSource representa um diretório de origem para backup.
type BackupSource struct {
	Path string `yaml:"path"`
}

// RetryInfo contém configurações de retry com exponential backoff.
type RetryInfo struct {
	MaxAttempts  int           `yaml:"max_attempts"`
	InitialDelay time.Duration `yaml:"initial_delay"`
	MaxDelay     time.Duration `yaml:"max_delay"`
}

// LoggingInfo contém configurações de logging.
type LoggingInfo struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// LoadAgentConfig lê e valida o arquivo YAML de configuração do agent.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading agent config: %w", err)
	}

	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing agent config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating agent config: %w", err)
	}

	return &cfg, nil
}

func (c *AgentConfig) validate() error {
	if c.Agent.Name == "" {
		return fmt.Errorf("agent.name is required")
	}
	if c.Daemon.Schedule == "" {
		return fmt.Errorf("daemon.schedule is required")
	}
	if c.Server.Address == "" {
		return fmt.Errorf("server.address is required")
	}
	if c.TLS.CACert == "" {
		return fmt.Errorf("tls.ca_cert is required")
	}
	if c.TLS.ClientCert == "" {
		return fmt.Errorf("tls.client_cert is required")
	}
	if c.TLS.ClientKey == "" {
		return fmt.Errorf("tls.client_key is required")
	}
	if len(c.Backups) == 0 {
		return fmt.Errorf("backups must have at least one entry")
	}
	for i, b := range c.Backups {
		if b.Name == "" {
			return fmt.Errorf("backups[%d].name is required", i)
		}
		if b.Storage == "" {
			return fmt.Errorf("backups[%d].storage is required", i)
		}
		if len(b.Sources) == 0 {
			return fmt.Errorf("backups[%d].sources must have at least one entry", i)
		}
		for j, src := range b.Sources {
			if src.Path == "" {
				return fmt.Errorf("backups[%d].sources[%d].path is required", i, j)
			}
		}
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = 5
	}
	if c.Retry.InitialDelay <= 0 {
		c.Retry.InitialDelay = 1 * time.Second
	}
	if c.Retry.MaxDelay <= 0 {
		c.Retry.MaxDelay = 5 * time.Minute
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	return nil
}
